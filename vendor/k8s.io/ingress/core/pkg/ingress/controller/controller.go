/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	api "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	unversionedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	fcache "k8s.io/client-go/tools/cache/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/ingress/core/pkg/ingress"
	"k8s.io/ingress/core/pkg/ingress/annotations/class"
	"k8s.io/ingress/core/pkg/ingress/annotations/healthcheck"
	"k8s.io/ingress/core/pkg/ingress/annotations/parser"
	"k8s.io/ingress/core/pkg/ingress/annotations/proxy"
	"k8s.io/ingress/core/pkg/ingress/defaults"
	"k8s.io/ingress/core/pkg/ingress/resolver"
	"k8s.io/ingress/core/pkg/ingress/status"
	"k8s.io/ingress/core/pkg/ingress/store"
	"k8s.io/ingress/core/pkg/k8s"
	"k8s.io/ingress/core/pkg/net/ssl"
	local_strings "k8s.io/ingress/core/pkg/strings"
	"k8s.io/ingress/core/pkg/task"
)

const (
	defUpstreamName = "upstream-default-backend"
	defServerName   = "_"
	rootLocation    = "/"

	fakeCertificate = "default-fake-certificate"
)

var (
	// list of ports that cannot be used by TCP or UDP services
	reservedPorts = []string{"80", "443", "8181", "18080"}

	fakeCertificatePath = ""
	fakeCertificateSHA  = ""

	cloner = conversion.NewCloner()
)

// GenericController holds the boilerplate code required to build an Ingress controlller.
type GenericController struct {
	cfg *Configuration

	ingController  cache.Controller
	endpController cache.Controller
	svcController  cache.Controller
	nodeController cache.Controller
	secrController cache.Controller
	mapController  cache.Controller

	ingLister  store.IngressLister
	svcLister  store.ServiceLister
	nodeLister store.NodeLister
	endpLister store.EndpointLister
	secrLister store.SecretLister
	mapLister  store.ConfigMapLister

	annotations annotationExtractor

	recorder record.EventRecorder

	syncQueue *task.Queue

	syncStatus status.Sync

	// local store of SSL certificates
	// (only certificates used in ingress)
	sslCertTracker *sslCertTracker

	syncRateLimiter flowcontrol.RateLimiter

	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock *sync.Mutex

	stopCh chan struct{}

	// runningConfig contains the running configuration in the Backend
	runningConfig *ingress.Configuration

	forceReload bool
}

// Configuration contains all the settings required by an Ingress controller
type Configuration struct {
	Client clientset.Interface

	ResyncPeriod   time.Duration
	DefaultService string
	IngressClass   string
	Namespace      string
	ConfigMapName  string

	ForceNamespaceIsolation bool
	DisableNodeList         bool

	// optional
	TCPConfigMapName string
	// optional
	UDPConfigMapName      string
	DefaultSSLCertificate string
	DefaultHealthzURL     string
	DefaultIngressClass   string
	// optional
	PublishService string
	// Backend is the particular implementation to be used.
	// (for instance NGINX)
	Backend ingress.Controller

	UpdateStatus           bool
	ElectionID             string
	UpdateStatusOnShutdown bool
	SortBackends           bool
}

// newIngressController creates an Ingress controller
func newIngressController(config *Configuration) *GenericController {

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&unversionedcore.EventSinkImpl{
		Interface: config.Client.CoreV1().Events(config.Namespace),
	})

	ic := GenericController{
		cfg:             config,
		stopLock:        &sync.Mutex{},
		stopCh:          make(chan struct{}),
		syncRateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.3, 1),
		recorder: eventBroadcaster.NewRecorder(scheme.Scheme, api.EventSource{
			Component: "ingress-controller",
		}),
		sslCertTracker: newSSLCertTracker(),
	}

	ic.syncQueue = task.NewTaskQueue(ic.syncIngress)

	// from here to the end of the method all the code is just boilerplate
	// required to watch Ingress, Secrets, ConfigMaps and Endoints.
	// This is used to detect new content, updates or removals and act accordingly
	ingEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			addIng := obj.(*extensions.Ingress)
			if !class.IsValid(addIng, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
				a, _ := parser.GetStringAnnotation(class.IngressKey, addIng)
				glog.Infof("ignoring add for ingress %v based on annotation %v with value %v", addIng.Name, class.IngressKey, a)
				return
			}
			ic.recorder.Eventf(addIng, api.EventTypeNormal, "CREATE", fmt.Sprintf("Ingress %s/%s", addIng.Namespace, addIng.Name))
			ic.syncQueue.Enqueue(obj)
		},
		DeleteFunc: func(obj interface{}) {
			delIng, ok := obj.(*extensions.Ingress)
			if !ok {
				// If we reached here it means the ingress was deleted but its final state is unrecorded.
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					glog.Errorf("couldn't get object from tombstone %#v", obj)
					return
				}
				delIng, ok = tombstone.Obj.(*extensions.Ingress)
				if !ok {
					glog.Errorf("Tombstone contained object that is not an Ingress: %#v", obj)
					return
				}
			}
			if !class.IsValid(delIng, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
				glog.Infof("ignoring delete for ingress %v based on annotation %v", delIng.Name, class.IngressKey)
				return
			}
			ic.recorder.Eventf(delIng, api.EventTypeNormal, "DELETE", fmt.Sprintf("Ingress %s/%s", delIng.Namespace, delIng.Name))
			ic.syncQueue.Enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			oldIng := old.(*extensions.Ingress)
			curIng := cur.(*extensions.Ingress)
			validOld := class.IsValid(oldIng, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass)
			validCur := class.IsValid(curIng, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass)
			if !validOld && validCur {
				glog.Infof("creating ingress %v based on annotation %v", curIng.Name, class.IngressKey)
				ic.recorder.Eventf(curIng, api.EventTypeNormal, "CREATE", fmt.Sprintf("Ingress %s/%s", curIng.Namespace, curIng.Name))
			} else if validOld && !validCur {
				glog.Infof("removing ingress %v based on annotation %v", curIng.Name, class.IngressKey)
				ic.recorder.Eventf(curIng, api.EventTypeNormal, "DELETE", fmt.Sprintf("Ingress %s/%s", curIng.Namespace, curIng.Name))
			} else if validCur && !reflect.DeepEqual(old, cur) {
				ic.recorder.Eventf(curIng, api.EventTypeNormal, "UPDATE", fmt.Sprintf("Ingress %s/%s", curIng.Namespace, curIng.Name))
			}

			ic.syncQueue.Enqueue(cur)
		},
	}

	secrEventHandler := cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				sec := cur.(*api.Secret)
				key := fmt.Sprintf("%v/%v", sec.Namespace, sec.Name)
				ic.syncSecret(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			sec, ok := obj.(*api.Secret)
			if !ok {
				// If we reached here it means the secret was deleted but its final state is unrecorded.
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					glog.Errorf("couldn't get object from tombstone %#v", obj)
					return
				}
				sec, ok = tombstone.Obj.(*api.Secret)
				if !ok {
					glog.Errorf("Tombstone contained object that is not a Secret: %#v", obj)
					return
				}
			}
			key := fmt.Sprintf("%v/%v", sec.Namespace, sec.Name)
			ic.sslCertTracker.DeleteAll(key)
		},
	}

	eventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ic.syncQueue.Enqueue(obj)
		},
		DeleteFunc: func(obj interface{}) {
			ic.syncQueue.Enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			oep := old.(*api.Endpoints)
			ocur := cur.(*api.Endpoints)
			if !reflect.DeepEqual(ocur.Subsets, oep.Subsets) {
				ic.syncQueue.Enqueue(cur)
			}
		},
	}

	mapEventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			upCmap := obj.(*api.ConfigMap)
			mapKey := fmt.Sprintf("%s/%s", upCmap.Namespace, upCmap.Name)
			if mapKey == ic.cfg.ConfigMapName {
				glog.V(2).Infof("adding configmap %v to backend", mapKey)
				ic.cfg.Backend.SetConfig(upCmap)
				ic.forceReload = true
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				upCmap := cur.(*api.ConfigMap)
				mapKey := fmt.Sprintf("%s/%s", upCmap.Namespace, upCmap.Name)
				if mapKey == ic.cfg.ConfigMapName {
					glog.V(2).Infof("updating configmap backend (%v)", mapKey)
					ic.cfg.Backend.SetConfig(upCmap)
					ic.forceReload = true
				}
				// updates to configuration configmaps can trigger an update
				if mapKey == ic.cfg.ConfigMapName || mapKey == ic.cfg.TCPConfigMapName || mapKey == ic.cfg.UDPConfigMapName {
					ic.recorder.Eventf(upCmap, api.EventTypeNormal, "UPDATE", fmt.Sprintf("ConfigMap %v", mapKey))
					ic.syncQueue.Enqueue(cur)
				}
			}
		},
	}

	watchNs := api.NamespaceAll
	if ic.cfg.ForceNamespaceIsolation && ic.cfg.Namespace != api.NamespaceAll {
		watchNs = ic.cfg.Namespace
	}

	ic.ingLister.Store, ic.ingController = cache.NewInformer(
		cache.NewListWatchFromClient(ic.cfg.Client.ExtensionsV1beta1().RESTClient(), "ingresses", ic.cfg.Namespace, fields.Everything()),
		&extensions.Ingress{}, ic.cfg.ResyncPeriod, ingEventHandler)

	ic.endpLister.Store, ic.endpController = cache.NewInformer(
		cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "endpoints", ic.cfg.Namespace, fields.Everything()),
		&api.Endpoints{}, ic.cfg.ResyncPeriod, eventHandler)

	ic.secrLister.Store, ic.secrController = cache.NewInformer(
		cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "secrets", watchNs, fields.Everything()),
		&api.Secret{}, ic.cfg.ResyncPeriod, secrEventHandler)

	ic.mapLister.Store, ic.mapController = cache.NewInformer(
		cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "configmaps", watchNs, fields.Everything()),
		&api.ConfigMap{}, ic.cfg.ResyncPeriod, mapEventHandler)

	ic.svcLister.Store, ic.svcController = cache.NewInformer(
		cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "services", ic.cfg.Namespace, fields.Everything()),
		&api.Service{}, ic.cfg.ResyncPeriod, cache.ResourceEventHandlerFuncs{})

	var nodeListerWatcher cache.ListerWatcher
	if config.DisableNodeList {
		nodeListerWatcher = fcache.NewFakeControllerSource()
	} else {
		nodeListerWatcher = cache.NewListWatchFromClient(ic.cfg.Client.CoreV1().RESTClient(), "nodes", api.NamespaceAll, fields.Everything())
	}
	ic.nodeLister.Store, ic.nodeController = cache.NewInformer(
		nodeListerWatcher,
		&api.Node{}, ic.cfg.ResyncPeriod, cache.ResourceEventHandlerFuncs{})

	if config.UpdateStatus {
		ic.syncStatus = status.NewStatusSyncer(status.Config{
			Client:                 config.Client,
			PublishService:         ic.cfg.PublishService,
			IngressLister:          ic.ingLister,
			ElectionID:             config.ElectionID,
			IngressClass:           config.IngressClass,
			DefaultIngressClass:    config.DefaultIngressClass,
			UpdateStatusOnShutdown: config.UpdateStatusOnShutdown,
			CustomIngressStatus:    ic.cfg.Backend.UpdateIngressStatus,
		})
	} else {
		glog.Warning("Update of ingress status is disabled (flag --update-status=false was specified)")
	}
	ic.annotations = newAnnotationExtractor(ic)

	ic.cfg.Backend.SetListers(ingress.StoreLister{
		Ingress:   ic.ingLister,
		Service:   ic.svcLister,
		Node:      ic.nodeLister,
		Endpoint:  ic.endpLister,
		Secret:    ic.secrLister,
		ConfigMap: ic.mapLister,
	})

	cloner.RegisterDeepCopyFunc(ingress.GetGeneratedDeepCopyFuncs)

	return &ic
}

// Info returns information about the backend
func (ic GenericController) Info() *ingress.BackendInfo {
	return ic.cfg.Backend.Info()
}

// IngressClass returns information about the backend
func (ic GenericController) IngressClass() string {
	return ic.cfg.IngressClass
}

// GetDefaultBackend returns the default backend
func (ic GenericController) GetDefaultBackend() defaults.Backend {
	return ic.cfg.Backend.BackendDefaults()
}

// GetRecorder returns the event recorder
func (ic GenericController) GetRecorder() record.EventRecorder {
	return ic.recorder
}

// GetSecret searches for a secret in the local secrets Store
func (ic GenericController) GetSecret(name string) (*api.Secret, error) {
	s, exists, err := ic.secrLister.Store.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("secret %v was not found", name)
	}
	return s.(*api.Secret), nil
}

// GetService searches for a service in the local secrets Store
func (ic GenericController) GetService(name string) (*api.Service, error) {
	s, exists, err := ic.svcLister.Store.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("service %v was not found", name)
	}
	return s.(*api.Service), nil
}

func (ic *GenericController) getConfigMap(ns, name string) (*api.ConfigMap, error) {
	s, exists, err := ic.mapLister.Store.GetByKey(fmt.Sprintf("%v/%v", ns, name))
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("configmap %v was not found", name)
	}
	return s.(*api.ConfigMap), nil
}

// sync collects all the pieces required to assemble the configuration file and
// then sends the content to the backend (OnUpdate) receiving the populated
// template as response reloading the backend if is required.
func (ic *GenericController) syncIngress(key interface{}) error {
	ic.syncRateLimiter.Accept()

	if ic.syncQueue.IsShuttingDown() {
		return nil
	}

	if name, ok := key.(string); ok {
		if obj, exists, _ := ic.ingLister.GetByKey(name); exists {
			ing := obj.(*extensions.Ingress)
			ic.readSecrets(ing)
		}
	}

	upstreams, servers := ic.getBackendServers()
	var passUpstreams []*ingress.SSLPassthroughBackend

	for _, server := range servers {
		if !server.SSLPassthrough {
			continue
		}

		for _, loc := range server.Locations {
			if loc.Path != rootLocation {
				glog.Warningf("ignoring path %v of ssl passthrough host %v", loc.Path, server.Hostname)
				continue
			}
			passUpstreams = append(passUpstreams, &ingress.SSLPassthroughBackend{
				Backend:  loc.Backend,
				Hostname: server.Hostname,
				Service:  loc.Service,
				Port:     loc.Port,
			})
			break
		}
	}

	pcfg := ingress.Configuration{
		Backends:            upstreams,
		Servers:             servers,
		TCPEndpoints:        ic.getStreamServices(ic.cfg.TCPConfigMapName, api.ProtocolTCP),
		UDPEndpoints:        ic.getStreamServices(ic.cfg.UDPConfigMapName, api.ProtocolUDP),
		PassthroughBackends: passUpstreams,
	}

	if !ic.forceReload && ic.runningConfig != nil && ic.runningConfig.Equal(&pcfg) {
		glog.V(3).Infof("skipping backend reload (no changes detected)")
		return nil
	}

	glog.Infof("backend reload required")

	err := ic.cfg.Backend.OnUpdate(pcfg)
	if err != nil {
		incReloadErrorCount()
		glog.Errorf("unexpected failure restarting the backend: \n%v", err)
		return err
	}

	glog.Infof("ingress backend successfully reloaded...")
	incReloadCount()
	setSSLExpireTime(servers)

	ic.runningConfig = &pcfg
	ic.forceReload = false

	return nil
}

func (ic *GenericController) getStreamServices(configmapName string, proto api.Protocol) []ingress.L4Service {
	glog.V(3).Infof("obtaining information about stream services of type %v located in configmap %v", proto, configmapName)
	if configmapName == "" {
		// no configmap configured
		return []ingress.L4Service{}
	}

	ns, name, err := k8s.ParseNameNS(configmapName)
	if err != nil {
		glog.Errorf("unexpected error reading configmap %v: %v", name, err)
		return []ingress.L4Service{}
	}

	configmap, err := ic.getConfigMap(ns, name)
	if err != nil {
		glog.Errorf("unexpected error reading configmap %v: %v", name, err)
		return []ingress.L4Service{}
	}

	var svcs []ingress.L4Service
	// k -> port to expose
	// v -> <namespace>/<service name>:<port from service to be used>
	for k, v := range configmap.Data {
		externalPort, err := strconv.Atoi(k)
		if err != nil {
			glog.Warningf("%v is not valid as a TCP/UDP port", k)
			continue
		}

		// this ports used by the backend
		if local_strings.StringInSlice(k, reservedPorts) {
			glog.Warningf("port %v cannot be used for TCP or UDP services. It is reserved for the Ingress controller", k)
			continue
		}

		nsSvcPort := strings.Split(v, ":")
		if len(nsSvcPort) < 2 {
			glog.Warningf("invalid format (namespace/name:port:[PROXY]) '%v'", k)
			continue
		}

		nsName := nsSvcPort[0]
		svcPort := nsSvcPort[1]
		useProxyProtocol := false

		// Proxy protocol is possible if the service is TCP
		if len(nsSvcPort) == 3 && proto == api.ProtocolTCP {
			if strings.ToUpper(nsSvcPort[2]) == "PROXY" {
				useProxyProtocol = true
			}
		}

		svcNs, svcName, err := k8s.ParseNameNS(nsName)
		if err != nil {
			glog.Warningf("%v", err)
			continue
		}

		svcObj, svcExists, err := ic.svcLister.Store.GetByKey(nsName)
		if err != nil {
			glog.Warningf("error getting service %v: %v", nsName, err)
			continue
		}

		if !svcExists {
			glog.Warningf("service %v was not found", nsName)
			continue
		}

		svc := svcObj.(*api.Service)

		var endps []ingress.Endpoint
		targetPort, err := strconv.Atoi(svcPort)
		if err != nil {
			glog.V(3).Infof("searching service %v/%v endpoints using the name '%v'", svcNs, svcName, svcPort)
			for _, sp := range svc.Spec.Ports {
				if sp.Name == svcPort {
					if sp.Protocol == proto {
						endps = ic.getEndpoints(svc, &sp, proto, &healthcheck.Upstream{})
						break
					}
				}
			}
		} else {
			// we need to use the TargetPort (where the endpoints are running)
			glog.V(3).Infof("searching service %v/%v endpoints using the target port '%v'", svcNs, svcName, targetPort)
			for _, sp := range svc.Spec.Ports {
				if sp.Port == int32(targetPort) {
					if sp.Protocol == proto {
						endps = ic.getEndpoints(svc, &sp, proto, &healthcheck.Upstream{})
						break
					}
				}
			}
		}

		// stream services cannot contain empty upstreams and there is no
		// default backend equivalent
		if len(endps) == 0 {
			glog.Warningf("service %v/%v does not have any active endpoints for port %v and protocol %v", svcNs, svcName, svcPort, proto)
			continue
		}

		svcs = append(svcs, ingress.L4Service{
			Port: externalPort,
			Backend: ingress.L4Backend{
				Name:             svcName,
				Namespace:        svcNs,
				Port:             intstr.FromString(svcPort),
				Protocol:         proto,
				UseProxyProtocol: useProxyProtocol,
			},
			Endpoints: endps,
		})
	}

	return svcs
}

// getDefaultUpstream returns an upstream associated with the
// default backend service. In case of error retrieving information
// configure the upstream to return http code 503.
func (ic *GenericController) getDefaultUpstream() *ingress.Backend {
	upstream := &ingress.Backend{
		Name: defUpstreamName,
	}
	svcKey := ic.cfg.DefaultService
	svcObj, svcExists, err := ic.svcLister.Store.GetByKey(svcKey)
	if err != nil {
		glog.Warningf("unexpected error searching the default backend %v: %v", ic.cfg.DefaultService, err)
		upstream.Endpoints = append(upstream.Endpoints, ic.cfg.Backend.DefaultEndpoint())
		return upstream
	}

	if !svcExists {
		glog.Warningf("service %v does not exist", svcKey)
		upstream.Endpoints = append(upstream.Endpoints, ic.cfg.Backend.DefaultEndpoint())
		return upstream
	}

	svc := svcObj.(*api.Service)
	endps := ic.getEndpoints(svc, &svc.Spec.Ports[0], api.ProtocolTCP, &healthcheck.Upstream{})
	if len(endps) == 0 {
		glog.Warningf("service %v does not have any active endpoints", svcKey)
		endps = []ingress.Endpoint{ic.cfg.Backend.DefaultEndpoint()}
	}

	upstream.Service = svc
	upstream.Endpoints = append(upstream.Endpoints, endps...)
	return upstream
}

type ingressByRevision []interface{}

func (c ingressByRevision) Len() int      { return len(c) }
func (c ingressByRevision) Swap(i, j int) { c[i], c[j] = c[j], c[i] }
func (c ingressByRevision) Less(i, j int) bool {
	ir := c[i].(*extensions.Ingress).ResourceVersion
	jr := c[j].(*extensions.Ingress).ResourceVersion
	return ir < jr
}

// getBackendServers returns a list of Upstream and Server to be used by the backend
// An upstream can be used in multiple servers if the namespace, service name and port are the same
func (ic *GenericController) getBackendServers() ([]*ingress.Backend, []*ingress.Server) {
	ings := ic.ingLister.Store.List()
	sort.Sort(ingressByRevision(ings))

	upstreams := ic.createUpstreams(ings)
	servers := ic.createServers(ings, upstreams)

	// If a server has a hostname equivalent to a pre-existing alias, then we
	// remove the alias to avoid conflicts.
	for _, server := range servers {
		for j, alias := range servers {
			if server.Hostname == alias.Alias {
				glog.Warningf("There is a conflict with server hostname '%v' and alias '%v' (in server %v). Removing alias to avoid conflicts.",
					server.Hostname, alias.Hostname, alias.Hostname)
				servers[j].Alias = ""
			}
		}
	}

	for _, ingIf := range ings {
		ing := ingIf.(*extensions.Ingress)

		affinity := ic.annotations.SessionAffinity(ing)

		if !class.IsValid(ing, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
			continue
		}

		anns := ic.annotations.Extract(ing)

		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" {
				host = defServerName
			}
			server := servers[host]
			if server == nil {
				server = servers[defServerName]
			}

			if rule.HTTP == nil &&
				host != defServerName {
				glog.V(3).Infof("ingress rule %v/%v does not contain HTTP rules, using default backend", ing.Namespace, ing.Name)
				continue
			}

			if server.CertificateAuth.CAFileName == "" {
				ca := ic.annotations.CertificateAuth(ing)
				if ca != nil {
					server.CertificateAuth = *ca
				}
			} else {
				glog.V(3).Infof("server %v already contains a muthual autentication configuration - ingress rule %v/%v", server.Hostname, ing.Namespace, ing.Name)
			}

			for _, path := range rule.HTTP.Paths {
				upsName := fmt.Sprintf("%v-%v-%v",
					ing.GetNamespace(),
					path.Backend.ServiceName,
					path.Backend.ServicePort.String())

				ups := upstreams[upsName]

				// if there's no path defined we assume /
				nginxPath := rootLocation
				if path.Path != "" {
					nginxPath = path.Path
				}

				addLoc := true
				for _, loc := range server.Locations {
					if loc.Path == nginxPath {
						addLoc = false

						if !loc.IsDefBackend {
							glog.V(3).Infof("avoiding replacement of ingress rule %v/%v location %v upstream %v (%v)", ing.Namespace, ing.Name, loc.Path, ups.Name, loc.Backend)
							break
						}

						glog.V(3).Infof("replacing ingress rule %v/%v location %v upstream %v (%v)", ing.Namespace, ing.Name, loc.Path, ups.Name, loc.Backend)
						loc.Backend = ups.Name
						loc.IsDefBackend = false
						loc.Backend = ups.Name
						loc.Port = ups.Port
						loc.Service = ups.Service
						loc.Ingress = ing
						mergeLocationAnnotations(loc, anns)
						if loc.Redirect.FromToWWW {
							server.RedirectFromToWWW = true
						}
						break
					}
				}
				// is a new location
				if addLoc {
					glog.V(3).Infof("adding location %v in ingress rule %v/%v upstream %v", nginxPath, ing.Namespace, ing.Name, ups.Name)
					loc := &ingress.Location{
						Path:         nginxPath,
						Backend:      ups.Name,
						IsDefBackend: false,
						Service:      ups.Service,
						Port:         ups.Port,
						Ingress:      ing,
					}
					mergeLocationAnnotations(loc, anns)
					if loc.Redirect.FromToWWW {
						server.RedirectFromToWWW = true
					}
					server.Locations = append(server.Locations, loc)
				}

				if ups.SessionAffinity.AffinityType == "" {
					ups.SessionAffinity.AffinityType = affinity.AffinityType
				}

				if affinity.AffinityType == "cookie" {
					ups.SessionAffinity.CookieSessionAffinity.Name = affinity.CookieConfig.Name
					ups.SessionAffinity.CookieSessionAffinity.Hash = affinity.CookieConfig.Hash

					locs := ups.SessionAffinity.CookieSessionAffinity.Locations
					if _, ok := locs[host]; !ok {
						locs[host] = []string{}
					}

					locs[host] = append(locs[host], path.Path)
				}
			}
		}
	}

	aUpstreams := make([]*ingress.Backend, 0, len(upstreams))

	for _, upstream := range upstreams {
		isHTTPSfrom := []*ingress.Server{}
		for _, server := range servers {
			for _, location := range server.Locations {
				if upstream.Name == location.Backend {
					if len(upstream.Endpoints) == 0 {
						glog.V(3).Infof("upstream %v does not have any active endpoints. Using default backend", upstream.Name)
						location.Backend = "upstream-default-backend"

						// check if the location contains endpoints and a custom default backend
						if location.DefaultBackend != nil {
							sp := location.DefaultBackend.Spec.Ports[0]
							endps := ic.getEndpoints(location.DefaultBackend, &sp, api.ProtocolTCP, &healthcheck.Upstream{})
							if len(endps) > 0 {
								glog.V(3).Infof("using custom default backend in server %v location %v (service %v/%v)",
									server.Hostname, location.Path, location.DefaultBackend.Namespace, location.DefaultBackend.Name)
								b, err := cloner.DeepCopy(upstream)
								if err != nil {
									glog.Errorf("unexpected error copying Upstream: %v", err)
								} else {
									name := fmt.Sprintf("custom-default-backend-%v", upstream.Name)
									nb := b.(*ingress.Backend)
									nb.Name = name
									nb.Endpoints = endps
									aUpstreams = append(aUpstreams, nb)
									location.Backend = name
								}
							}
						}
					}

					// Configure Backends[].SSLPassthrough
					if server.SSLPassthrough {
						if location.Path == rootLocation {
							if location.Backend == defUpstreamName {
								glog.Warningf("ignoring ssl passthrough of %v as it doesn't have a default backend (root context)", server.Hostname)
								continue
							}

							isHTTPSfrom = append(isHTTPSfrom, server)
						}
					}
				}
			}
		}

		if len(isHTTPSfrom) > 0 {
			upstream.SSLPassthrough = true
		}
	}

	// create the list of upstreams and skip those without endpoints
	for _, upstream := range upstreams {
		if len(upstream.Endpoints) == 0 {
			continue
		}
		aUpstreams = append(aUpstreams, upstream)
	}

	if ic.cfg.SortBackends {
		sort.Sort(ingress.BackendByNameServers(aUpstreams))
	}

	aServers := make([]*ingress.Server, 0, len(servers))
	for _, value := range servers {
		sort.Sort(ingress.LocationByPath(value.Locations))
		aServers = append(aServers, value)
	}
	sort.Sort(ingress.ServerByName(aServers))

	return aUpstreams, aServers
}

// GetAuthCertificate ...
func (ic GenericController) GetAuthCertificate(secretName string) (*resolver.AuthSSLCert, error) {
	if _, exists := ic.sslCertTracker.Get(secretName); !exists {
		ic.syncSecret(secretName)
	}

	_, err := ic.GetSecret(secretName)
	if err != nil {
		return &resolver.AuthSSLCert{}, fmt.Errorf("unexpected error: %v", err)
	}

	bc, exists := ic.sslCertTracker.Get(secretName)
	if !exists {
		return &resolver.AuthSSLCert{}, fmt.Errorf("secret %v does not exist", secretName)
	}
	cert := bc.(*ingress.SSLCert)
	return &resolver.AuthSSLCert{
		Secret:     secretName,
		CAFileName: cert.CAFileName,
		PemSHA:     cert.PemSHA,
	}, nil
}

// createUpstreams creates the NGINX upstreams for each service referenced in
// Ingress rules. The servers inside the upstream are endpoints.
func (ic *GenericController) createUpstreams(data []interface{}) map[string]*ingress.Backend {
	upstreams := make(map[string]*ingress.Backend)
	upstreams[defUpstreamName] = ic.getDefaultUpstream()

	for _, ingIf := range data {
		ing := ingIf.(*extensions.Ingress)

		if !class.IsValid(ing, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
			continue
		}

		secUpstream := ic.annotations.SecureUpstream(ing)
		hz := ic.annotations.HealthCheck(ing)
		serviceUpstream := ic.annotations.ServiceUpstream(ing)

		var defBackend string
		if ing.Spec.Backend != nil {
			defBackend = fmt.Sprintf("%v-%v-%v",
				ing.GetNamespace(),
				ing.Spec.Backend.ServiceName,
				ing.Spec.Backend.ServicePort.String())

			glog.V(3).Infof("creating upstream %v", defBackend)
			upstreams[defBackend] = newUpstream(defBackend)
			svcKey := fmt.Sprintf("%v/%v", ing.GetNamespace(), ing.Spec.Backend.ServiceName)

			// Add the service cluster endpoint as the upstream instead of individual endpoints
			// if the serviceUpstream annotation is enabled
			if serviceUpstream {
				endpoint, err := ic.getServiceClusterEndpoint(svcKey, ing.Spec.Backend)
				if err != nil {
					glog.Errorf("Failed to get service cluster endpoint for service %s: %v", svcKey, err)
				} else {
					upstreams[defBackend].Endpoints = []ingress.Endpoint{endpoint}
				}
			}

			if len(upstreams[defBackend].Endpoints) == 0 {
				endps, err := ic.serviceEndpoints(svcKey, ing.Spec.Backend.ServicePort.String(), hz)
				upstreams[defBackend].Endpoints = append(upstreams[defBackend].Endpoints, endps...)
				if err != nil {
					glog.Warningf("error creating upstream %v: %v", defBackend, err)
				}
			}

		}

		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}

			for _, path := range rule.HTTP.Paths {
				name := fmt.Sprintf("%v-%v-%v",
					ing.GetNamespace(),
					path.Backend.ServiceName,
					path.Backend.ServicePort.String())

				if _, ok := upstreams[name]; ok {
					continue
				}

				glog.V(3).Infof("creating upstream %v", name)
				upstreams[name] = newUpstream(name)
				upstreams[name].Port = path.Backend.ServicePort

				if !upstreams[name].Secure {
					upstreams[name].Secure = secUpstream.Secure
				}

				if upstreams[name].SecureCACert.Secret == "" {
					upstreams[name].SecureCACert = secUpstream.CACert
				}

				svcKey := fmt.Sprintf("%v/%v", ing.GetNamespace(), path.Backend.ServiceName)

				// Add the service cluster endpoint as the upstream instead of individual endpoints
				// if the serviceUpstream annotation is enabled
				if serviceUpstream {
					endpoint, err := ic.getServiceClusterEndpoint(svcKey, &path.Backend)
					if err != nil {
						glog.Errorf("failed to get service cluster endpoint for service %s: %v", svcKey, err)
					} else {
						upstreams[name].Endpoints = []ingress.Endpoint{endpoint}
					}
				}

				if len(upstreams[name].Endpoints) == 0 {
					endp, err := ic.serviceEndpoints(svcKey, path.Backend.ServicePort.String(), hz)
					if err != nil {
						glog.Warningf("error obtaining service endpoints: %v", err)
						continue
					}
					upstreams[name].Endpoints = endp
				}

				s, exists, err := ic.svcLister.Store.GetByKey(svcKey)
				if err != nil {
					glog.Warningf("error obtaining service: %v", err)
					continue
				}

				if !exists {
					glog.Warningf("service %v does not exists", svcKey)
					continue
				}

				upstreams[name].Service = s.(*api.Service)
			}
		}
	}

	return upstreams
}

func (ic *GenericController) getServiceClusterEndpoint(svcKey string, backend *extensions.IngressBackend) (endpoint ingress.Endpoint, err error) {
	svcObj, svcExists, err := ic.svcLister.Store.GetByKey(svcKey)

	if !svcExists {
		return endpoint, fmt.Errorf("service %v does not exist", svcKey)
	}

	svc := svcObj.(*api.Service)
	if svc.Spec.ClusterIP == "" {
		return endpoint, fmt.Errorf("No ClusterIP found for service %s", svcKey)
	}

	endpoint.Address = svc.Spec.ClusterIP
	endpoint.Port = backend.ServicePort.String()

	return endpoint, err
}

// serviceEndpoints returns the upstream servers (endpoints) associated
// to a service.
func (ic *GenericController) serviceEndpoints(svcKey, backendPort string,
	hz *healthcheck.Upstream) ([]ingress.Endpoint, error) {
	svcObj, svcExists, err := ic.svcLister.Store.GetByKey(svcKey)

	var upstreams []ingress.Endpoint
	if err != nil {
		return upstreams, fmt.Errorf("error getting service %v from the cache: %v", svcKey, err)
	}

	if !svcExists {
		err = fmt.Errorf("service %v does not exist", svcKey)
		return upstreams, err
	}

	svc := svcObj.(*api.Service)
	glog.V(3).Infof("obtaining port information for service %v", svcKey)
	for _, servicePort := range svc.Spec.Ports {
		// targetPort could be a string, use the name or the port (int)
		if strconv.Itoa(int(servicePort.Port)) == backendPort ||
			servicePort.TargetPort.String() == backendPort ||
			servicePort.Name == backendPort {

			endps := ic.getEndpoints(svc, &servicePort, api.ProtocolTCP, hz)
			if len(endps) == 0 {
				glog.Warningf("service %v does not have any active endpoints", svcKey)
			}

			if ic.cfg.SortBackends {
				sort.Sort(ingress.EndpointByAddrPort(endps))
			}
			upstreams = append(upstreams, endps...)
			break
		}
	}

	if !ic.cfg.SortBackends {
		rand.Seed(time.Now().UnixNano())
		for i := range upstreams {
			j := rand.Intn(i + 1)
			upstreams[i], upstreams[j] = upstreams[j], upstreams[i]
		}
	}

	return upstreams, nil
}

// createServers initializes a map that contains information about the list of
// FDQN referenced by ingress rules and the common name field in the referenced
// SSL certificates. Each server is configured with location / using a default
// backend specified by the user or the one inside the ingress spec.
func (ic *GenericController) createServers(data []interface{},
	upstreams map[string]*ingress.Backend) map[string]*ingress.Server {
	servers := make(map[string]*ingress.Server)

	bdef := ic.GetDefaultBackend()
	ngxProxy := proxy.Configuration{
		BodySize:         bdef.ProxyBodySize,
		ConnectTimeout:   bdef.ProxyConnectTimeout,
		SendTimeout:      bdef.ProxySendTimeout,
		ReadTimeout:      bdef.ProxyReadTimeout,
		BufferSize:       bdef.ProxyBufferSize,
		CookieDomain:     bdef.ProxyCookieDomain,
		CookiePath:       bdef.ProxyCookiePath,
		NextUpstream:     bdef.ProxyNextUpstream,
		RequestBuffering: bdef.ProxyRequestBuffering,
	}

	defaultPemFileName := fakeCertificatePath
	defaultPemSHA := fakeCertificateSHA

	// Tries to fetch the default Certificate. If it does not exists, generate a new self signed one.
	defaultCertificate, err := ic.getPemCertificate(ic.cfg.DefaultSSLCertificate)
	if err == nil {
		defaultPemFileName = defaultCertificate.PemFileName
		defaultPemSHA = defaultCertificate.PemSHA
	}

	// initialize the default server
	du := ic.getDefaultUpstream()
	servers[defServerName] = &ingress.Server{
		Hostname:       defServerName,
		SSLCertificate: defaultPemFileName,
		SSLPemChecksum: defaultPemSHA,
		Locations: []*ingress.Location{
			{
				Path:         rootLocation,
				IsDefBackend: true,
				Backend:      du.Name,
				Proxy:        ngxProxy,
				Service:      du.Service,
			},
		}}

	// initialize all the servers
	for _, ingIf := range data {
		ing := ingIf.(*extensions.Ingress)
		if !class.IsValid(ing, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
			continue
		}

		// check if ssl passthrough is configured
		sslpt := ic.annotations.SSLPassthrough(ing)
		du := ic.getDefaultUpstream()
		un := du.Name
		if ing.Spec.Backend != nil {
			// replace default backend
			defUpstream := fmt.Sprintf("%v-%v-%v", ing.GetNamespace(), ing.Spec.Backend.ServiceName, ing.Spec.Backend.ServicePort.String())
			if backendUpstream, ok := upstreams[defUpstream]; ok {
				un = backendUpstream.Name
			}
		}

		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" {
				host = defServerName
			}
			if _, ok := servers[host]; ok {
				// server already configured
				continue
			}

			servers[host] = &ingress.Server{
				Hostname: host,
				Locations: []*ingress.Location{
					{
						Path:         rootLocation,
						IsDefBackend: true,
						Backend:      un,
						Proxy:        ngxProxy,
						Service:      &api.Service{},
					},
				}, SSLPassthrough: sslpt}
		}
	}

	// configure default location, alias, and SSL
	for _, ingIf := range data {
		ing := ingIf.(*extensions.Ingress)
		if !class.IsValid(ing, ic.cfg.IngressClass, ic.cfg.DefaultIngressClass) {
			continue
		}

		// setup server-alias based on annotations
		aliasAnnotation := ic.annotations.Alias(ing)

		for _, rule := range ing.Spec.Rules {
			host := rule.Host
			if host == "" {
				host = defServerName
			}

			// setup server aliases
			servers[host].Alias = aliasAnnotation

			// only add a certificate if the server does not have one previously configured
			if servers[host].SSLCertificate != "" {
				continue
			}

			if len(ing.Spec.TLS) == 0 {
				glog.V(3).Infof("ingress %v/%v for host %v does not contains a TLS section", ing.Namespace, ing.Name, host)
				continue
			}

			tlsSecretName := ""
			found := false
			for _, tls := range ing.Spec.TLS {
				if sets.NewString(tls.Hosts...).Has(host) {
					tlsSecretName = tls.SecretName
					found = true
					break
				}
			}

			if !found {
				glog.Warningf("ingress %v/%v for host %v contains a TLS section but none of the host match",
					ing.Namespace, ing.Name, host)
				continue
			}

			if tlsSecretName == "" {
				glog.V(3).Infof("host %v is listed on tls section but secretName is empty. Using default cert", host)
				servers[host].SSLCertificate = defaultPemFileName
				servers[host].SSLPemChecksum = defaultPemSHA
				continue
			}

			key := fmt.Sprintf("%v/%v", ing.Namespace, tlsSecretName)
			bc, exists := ic.sslCertTracker.Get(key)
			if !exists {
				glog.Warningf("ssl certificate \"%v\" does not exist in local store", key)
				continue
			}

			cert := bc.(*ingress.SSLCert)
			err = cert.Certificate.VerifyHostname(host)
			if err != nil {
				glog.Warningf("ssl certificate %v does not contain a Common Name or Subject Alternative Name for host %v", key, host)
				continue
			}

			servers[host].SSLCertificate = cert.PemFileName
			servers[host].SSLPemChecksum = cert.PemSHA
			servers[host].SSLExpireTime = cert.ExpireTime

			if cert.ExpireTime.Before(time.Now().Add(240 * time.Hour)) {
				glog.Warningf("ssl certificate for host %v is about to expire in 10 days", host)
			}
		}
	}

	return servers
}

// getEndpoints returns a list of <endpoint ip>:<port> for a given service/target port combination.
func (ic *GenericController) getEndpoints(
	s *api.Service,
	servicePort *api.ServicePort,
	proto api.Protocol,
	hz *healthcheck.Upstream) []ingress.Endpoint {

	upsServers := []ingress.Endpoint{}

	// avoid duplicated upstream servers when the service
	// contains multiple port definitions sharing the same
	// targetport.
	adus := make(map[string]bool)

	// ExternalName services
	if s.Spec.Type == api.ServiceTypeExternalName {
		targetPort := servicePort.TargetPort.IntValue()
		// check for invalid port value
		if targetPort <= 0 {
			return upsServers
		}

		return append(upsServers, ingress.Endpoint{
			Address:     s.Spec.ExternalName,
			Port:        fmt.Sprintf("%v", targetPort),
			MaxFails:    hz.MaxFails,
			FailTimeout: hz.FailTimeout,
		})
	}

	glog.V(3).Infof("getting endpoints for service %v/%v and port %v", s.Namespace, s.Name, servicePort.String())
	ep, err := ic.endpLister.GetServiceEndpoints(s)
	if err != nil {
		glog.Warningf("unexpected error obtaining service endpoints: %v", err)
		return upsServers
	}

	for _, ss := range ep.Subsets {
		for _, epPort := range ss.Ports {

			if !reflect.DeepEqual(epPort.Protocol, proto) {
				continue
			}

			var targetPort int32

			if servicePort.Name == "" {
				// ServicePort.Name is optional if there is only one port
				targetPort = epPort.Port
			} else if servicePort.Name == epPort.Name {
				targetPort = epPort.Port
			}

			// check for invalid port value
			if targetPort <= 0 {
				continue
			}

			for _, epAddress := range ss.Addresses {
				ep := fmt.Sprintf("%v:%v", epAddress.IP, targetPort)
				if _, exists := adus[ep]; exists {
					continue
				}
				ups := ingress.Endpoint{
					Address:     epAddress.IP,
					Port:        fmt.Sprintf("%v", targetPort),
					MaxFails:    hz.MaxFails,
					FailTimeout: hz.FailTimeout,
					Target:      epAddress.TargetRef,
				}
				upsServers = append(upsServers, ups)
				adus[ep] = true
			}
		}
	}

	glog.V(3).Infof("endpoints found: %v", upsServers)
	return upsServers
}

// readSecrets extracts information about secrets from an Ingress rule
func (ic *GenericController) readSecrets(ing *extensions.Ingress) {
	for _, tls := range ing.Spec.TLS {
		if tls.SecretName == "" {
			continue
		}

		key := fmt.Sprintf("%v/%v", ing.Namespace, tls.SecretName)
		ic.syncSecret(key)
	}

	key, _ := parser.GetStringAnnotation("ingress.kubernetes.io/auth-tls-secret", ing)
	if key == "" {
		return
	}
	ic.syncSecret(key)
}

// Stop stops the loadbalancer controller.
func (ic GenericController) Stop() error {
	ic.stopLock.Lock()
	defer ic.stopLock.Unlock()

	// Only try draining the workqueue if we haven't already.
	if !ic.syncQueue.IsShuttingDown() {
		glog.Infof("shutting down controller queues")
		close(ic.stopCh)
		go ic.syncQueue.Shutdown()
		if ic.syncStatus != nil {
			ic.syncStatus.Shutdown()
		}
		return nil
	}

	return fmt.Errorf("shutdown already in progress")
}

// Start starts the Ingress controller.
func (ic GenericController) Start() {
	glog.Infof("starting Ingress controller")

	go ic.ingController.Run(ic.stopCh)
	go ic.endpController.Run(ic.stopCh)
	go ic.svcController.Run(ic.stopCh)
	go ic.nodeController.Run(ic.stopCh)
	go ic.secrController.Run(ic.stopCh)
	go ic.mapController.Run(ic.stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(ic.stopCh,
		ic.ingController.HasSynced,
		ic.svcController.HasSynced,
		ic.endpController.HasSynced,
		ic.secrController.HasSynced,
		ic.mapController.HasSynced,
		ic.nodeController.HasSynced,
	) {
		runtime.HandleError(fmt.Errorf("Timed out waiting for caches to sync"))
	}

	// initial sync of secrets to avoid unnecessary reloads
	for _, key := range ic.ingLister.ListKeys() {
		if obj, exists, _ := ic.ingLister.GetByKey(key); exists {
			ing := obj.(*extensions.Ingress)
			ic.readSecrets(ing)
		}
	}

	createDefaultSSLCertificate()

	go ic.syncQueue.Run(time.Second, ic.stopCh)

	if ic.syncStatus != nil {
		go ic.syncStatus.Run(ic.stopCh)
	}

	<-ic.stopCh
}

func createDefaultSSLCertificate() {
	defCert, defKey := ssl.GetFakeSSLCert()
	c, err := ssl.AddOrUpdateCertAndKey(fakeCertificate, defCert, defKey, []byte{})
	if err != nil {
		glog.Fatalf("Error generating self signed certificate: %v", err)
	}

	fakeCertificateSHA = c.PemSHA
	fakeCertificatePath = c.PemFileName
}
