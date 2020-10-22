package kube

import (
	"demoless/provider"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
)

const (
	annotationKey = "demoless/app"
)

func getClientSet() (*kubernetes.Clientset, error) {
	var c *rest.Config
	c, err := rest.InClusterConfig()
	if err != nil && err == rest.ErrNotInCluster {
		c, err = clientcmd.BuildConfigFromFlags("", filepath.Join(os.Getenv("HOME"), ".kube", "config"))
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(c)
}

type KubeProvider struct {
	sync.RWMutex
	client          kubernetes.Interface
	informerFactory informers.SharedInformerFactory
	si              coreinformers.ServiceInformer
	di              appsinformers.DeploymentInformer
	epi             coreinformers.EndpointsInformer

	namespace         string
	sycnBackendsQueue workqueue.Interface
	backends          map[string]*KubeBackend
}

func NewKubeProvider(namespace string) provider.Provider {
	clientset, err := getClientSet()
	if err != nil {
		logrus.Fatalf("failed to get k8s clientset: %v", err)
		return nil
	}
	informerFactory := informers.NewSharedInformerFactory(clientset, 0)
	kp := &KubeProvider{
		client:            clientset,
		informerFactory:   informerFactory,
		si:                informerFactory.Core().V1().Services(),
		di:                informerFactory.Apps().V1().Deployments(),
		epi:               informerFactory.Core().V1().Endpoints(),
		namespace:         namespace,
		backends:          make(map[string]*KubeBackend),
		sycnBackendsQueue: workqueue.New(),
	}
	return kp
}

func (kp *KubeProvider) Name() provider.ProviderType {
	return provider.ProviderTypeKube
}

func (kp *KubeProvider) Run(stop chan struct{}) error {
	logrus.Info("starting informer")
	now := time.Now()
	go kp.si.Informer().Run(stop)
	go kp.di.Informer().Run(stop)
	go kp.epi.Informer().Run(stop)
	logrus.Info("wait for informer cache sync")

	if !cache.WaitForCacheSync(stop,
		kp.si.Informer().HasSynced,
		kp.di.Informer().HasSynced,
		kp.epi.Informer().HasSynced,
	) {
		return errors.New("failed to sync informer cache")
	}
	logrus.Infof("informer cache synced, duration: %v", time.Since(now))
	if err := kp.ServiceDiscovery(stop); err != nil {
		logrus.Errorf("start kubernetes service discovery with error: %v", err)
	}
	return nil
}

func (kp *KubeProvider) Find(id string) (provider.Backend, error) {
	kp.RLock()
	defer kp.RUnlock()
	be, ok := kp.backends[id]
	if !ok {
		return nil, fmt.Errorf("kubernetes backend %s not found", id)
	}
	return be, nil
}

func (kp *KubeProvider) List() (list []provider.Backend) {
	for _, b := range kp.backends {
		list = append(list, b)
	}
	return
}

func (kp *KubeProvider) ServiceDiscovery(stop chan struct{}) error {
	// 自动发现后端服务列表
	svcHandlerFunc := func(obj interface{}) {
		service := obj.(*corev1.Service)
		app := service.Labels["app"]
		appAnno := service.Annotations[annotationKey]
		if service.Namespace == kp.namespace && app == appAnno && appAnno != "" {
			kp.sycnBackendsQueue.Add(app)
		}
	}
	kp.si.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			svcHandlerFunc(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			svcHandlerFunc(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			svcHandlerFunc(obj)
		},
	})
	deployHandlerFunc := func(obj interface{}) {
		deploy := obj.(*appsv1.Deployment)
		app := deploy.Labels["app"]
		appAnno := deploy.Annotations[annotationKey]
		if deploy.Namespace == kp.namespace && app == appAnno && appAnno != "" {
			kp.sycnBackendsQueue.Add(app)
		}
	}
	kp.di.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			deployHandlerFunc(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			deployHandlerFunc(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			deployHandlerFunc(obj)
		},
	})
	endpointsHandlerFunc := func(obj interface{}) {
		endpoints := obj.(*corev1.Endpoints)
		app := endpoints.Labels["app"]
		if endpoints.Namespace == kp.namespace && app != "" {
			kp.sycnBackendsQueue.Add(app)
		}
	}
	kp.epi.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			endpointsHandlerFunc(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			endpointsHandlerFunc(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			endpointsHandlerFunc(obj)
		},
	})
	logrus.Infof("start k8s sd workers")
	for i := 0; i < 5; i++ {
		go wait.Until(func() {
			for kp.processSyncItem() {
			}
		}, 1*time.Second, stop)
	}

	<-stop
	logrus.Infof("shutting down k8s sd workers")
	return nil
}

// processSyncItem 监听到所有 Pod、Service、Deployment 变化并同步其 app 状态
func (kp *KubeProvider) processSyncItem() bool {
	item, quit := kp.sycnBackendsQueue.Get()
	if quit {
		return false
	}
	app := item.(string)
	defer kp.sycnBackendsQueue.Done(item)
	if err := kp.syncBackend(app); err != nil {
		logrus.Warnf("sync kubernetes backend %s with error: %v", app, err)
	}
	return true
}

// syncBackend 同步一个app的状态，若存在且资源没有问题则同步其健康状态
// 若其关联资源有问题则从backends map中删除此app
// 每次pod/service/deployment资源变化都会执行此方法
func (kp *KubeProvider) syncBackend(app string) error {
	now := time.Now()
	logrus.Infof("sync app: start sycning %s", app)
	defer func() {
		logrus.Infof("sync app: complete %s, duration %v", app, time.Since(now))
		if be, ok := kp.backends[app]; ok && be.Available() {
			be.cond.Broadcast()
		}
	}()
	var parsedURL *url.URL
	stateHealthy := false
	stateStarting := false
	// check deployment
	deployment, err := kp.di.Lister().Deployments(kp.namespace).Get(app)
	if err != nil {
		logrus.Warnf("get deployment %s error, %v", app, err)
		kp.removeBackend(app)
		return err
	}
	if *deployment.Spec.Replicas == 0 {
		stateStarting = false
		stateHealthy = false
	}
	// check endpoints
	ep, err := kp.epi.Lister().Endpoints(kp.namespace).Get(app)
	if err != nil {
		logrus.Warnf("get endpointes %s error, %v", app, err)
		kp.removeBackend(app)
		return err
	}
	if len(ep.Subsets) != 0 {
		stateHealthy = true
	} else if len(ep.Subsets) == 0 && *deployment.Spec.Replicas != 0 {
		stateHealthy = false
		stateStarting = true
	}

	// check service
	service, err := kp.si.Lister().Services(kp.namespace).Get(app)
	if err != nil {
		logrus.Warnf("get service %s error, %v", app, err)
		kp.removeBackend(app)
		return err
	}
	// 直接请求 Service Name
	if len(service.Spec.Ports) == 0 {
		logrus.Warnf("service %s has no port", app)
		kp.removeBackend(app)
		return fmt.Errorf("service %s has no port", app)
	}
	u, err := url.Parse(fmt.Sprintf("http://%s:%d", service.Name, service.Spec.Ports[0].Port))
	if err != nil {
		logrus.Warnf("parse service %s IP err: %v", app, err)
		kp.removeBackend(app)
		return err
	}
	parsedURL = u

	var eps []*url.URL
	for _, e := range ep.Subsets {
		if len(e.Addresses) > 0 && len(e.Ports) > 0 {
			urlStr := fmt.Sprintf("http://%s:%d", e.Addresses[0].IP, e.Ports[0].Port)
			u, err := url.Parse(urlStr)
			if err != nil {
				logrus.Warnf("parse url %s with error: %v", urlStr, err)
				continue
			}
			eps = append(eps, u)
		}
	}
	// update kp.backends
	kp.Lock()
	defer kp.Unlock()
	_, ok := kp.backends[app]
	if !ok {
		// new backend
		logrus.Infof("sync app: new app %s", app)
		kp.backends[app] = &KubeBackend{
			RWMutex:       sync.RWMutex{},
			cond:          sync.NewCond(&sync.Mutex{}),
			manager:       kp,
			name:          app,
			url:           parsedURL,
			eps:           eps,
			stateHealthy:  stateHealthy,
			stateStarting: stateStarting,
		}
	} else {
		// update existing backend
		logrus.Infof("sync app: update app %s", app)
		kp.backends[app].name = app
		kp.backends[app].url = parsedURL
		kp.backends[app].eps = eps
		kp.backends[app].stateHealthy = stateHealthy
		kp.backends[app].stateStarting = stateStarting
	}
	return nil
}

func (kp *KubeProvider) removeBackend(app string) {
	kp.Lock()
	defer kp.Unlock()
	delete(kp.backends, app)
}
