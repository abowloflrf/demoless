package kube

import (
	"context"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	// TODO: 为此类 app 的资源都设置一个 annotation
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
	pi              coreinformers.PodInformer
	di              appsinformers.DeploymentInformer

	namespace         string
	sycnBackendsQueue workqueue.Interface
	backends          map[string]*KubeBackend
}

// KubeBackend k8s 提供后端服务，deployment + service
type KubeBackend struct {
	sync.RWMutex
	cond          *sync.Cond
	manager       *KubeProvider
	name          string   // deployment/service name
	url           *url.URL // parsed service address
	stateHealthy  bool     // 后端有至少一个 Ready 的 Pod
	stateStarting bool     // Deployment 下实例数非0，且还没有ready的Pod
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
		pi:                informerFactory.Core().V1().Pods(),
		di:                informerFactory.Apps().V1().Deployments(),
		namespace:         namespace,
		backends:          make(map[string]*KubeBackend),
		sycnBackendsQueue: workqueue.New(),
	}
	return kp
}

func (kp *KubeProvider) Run(stop chan struct{}) error {
	logrus.Info("starting informer")
	now := time.Now()
	go kp.si.Informer().Run(stop)
	go kp.pi.Informer().Run(stop)
	go kp.di.Informer().Run(stop)
	logrus.Info("wait for informer cache sync")
	if !cache.WaitForCacheSync(stop, kp.si.Informer().HasSynced, kp.pi.Informer().HasSynced, kp.di.Informer().HasSynced) {
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

func (kp *KubeProvider) ServiceDiscovery(stop chan struct{}) error {
	// TODO: 自动发现后端服务列表，目前写死一个
	// init
	apps := []string{"demoapp"}
	for _, app := range apps {
		if err := kp.syncBackend(app); err != nil {
			logrus.Warnf("sync kubernetes app %s with error: %v", app, err)
		}
	}
	// register event handler
	svcHandlerFunc := func(obj interface{}) {
		service := obj.(corev1.Service)
		app, ok := service.Labels["app"]
		if !ok || app != service.Name {
			return
		}
		kp.sycnBackendsQueue.Add(app)
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
		deploy := obj.(appsv1.Deployment)
		app, ok := deploy.Labels["app"]
		if !ok || app != deploy.Name {
			return
		}
		kp.sycnBackendsQueue.Add(app)
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
	podHandlerFunc := func(obj interface{}) {
		pod := obj.(corev1.Pod)
		app, ok := pod.Labels["app"]
		if !ok || app != pod.Name {
			return
		}
		kp.sycnBackendsQueue.Add(app)
	}
	kp.pi.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			podHandlerFunc(obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			podHandlerFunc(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			podHandlerFunc(obj)
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
	defer func() {
		if be, ok := kp.backends[app]; ok {
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
	// check pods
	pods, err := kp.pi.Lister().Pods(kp.namespace).List(labels.SelectorFromSet(map[string]string{"app": app}))
	if err != nil {
		logrus.Warnf("get gets %s error, %v", app, err)
		kp.removeBackend(app)
		return err
	}
	for _, pod := range pods {
		ready := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if ready {
			stateHealthy = true
		}
	}
	if len(pods) != 0 && !stateHealthy {
		stateStarting = true
	}

	// check service
	service, err := kp.si.Lister().Services(kp.namespace).Get(app)
	if err != nil {
		logrus.Warnf("get service %s error, %v", app, err)
		kp.removeBackend(app)
		return err
	}
	u, err := url.Parse(service.Spec.ClusterIP)
	if err != nil {
		logrus.Warnf("parse service %s IP err: %v", app, err)
		kp.removeBackend(app)
		return err
	}
	parsedURL = u

	// update kp.backends
	kp.Lock()
	defer kp.Unlock()
	_, ok := kp.backends[app]
	if !ok {
		// new backend
		kp.backends[app] = &KubeBackend{
			RWMutex:       sync.RWMutex{},
			cond:          sync.NewCond(&sync.Mutex{}),
			manager:       kp,
			name:          app,
			url:           parsedURL,
			stateHealthy:  stateHealthy,
			stateStarting: stateStarting,
		}
	} else {
		// update existing backend
		kp.backends[app].name = app
		kp.backends[app].url = parsedURL
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

func (kb *KubeBackend) ID() string {
	return kb.name
}

func (kb *KubeBackend) Addr() *url.URL {
	return kb.url
}

func (kb *KubeBackend) Available() bool {
	kb.RLock()
	defer kb.RUnlock()
	return kb.stateHealthy
}

// Instance 返回其关联 Deployment 的副本数量
func (kb *KubeBackend) Instance() int {
	deployment, err := kb.manager.di.Lister().Deployments(kb.manager.namespace).Get(kb.ID())
	if err != nil {
		if err := kb.manager.syncBackend(kb.ID()); err != nil {
			logrus.Warnf("sycn app with error: %v", err)
		}
		return 0
	}
	return deployment.Size()
}

// Unfreeze 设置目标 deployment 副本数为 1
func (kb *KubeBackend) Unfreeze() error {
	deployment, err := kb.manager.di.Lister().Deployments(kb.manager.namespace).Get(kb.ID())
	if err != nil {
		if err := kb.manager.syncBackend(kb.ID()); err != nil {
			logrus.Warnf("sycn app with error: %v", err)
		}
		return err
	}
	if deployment.Size() != 0 {
		return nil
	}
	var targetReplica int32 = 1
	deployment.Spec.Replicas = &targetReplica
	_, err = kb.manager.client.AppsV1().Deployments(kb.manager.namespace).
		Update(context.Background(), deployment, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	kb.Lock()
	kb.stateStarting = true
	kb.stateHealthy = false
	kb.Unlock()
	return nil
}

func (kb *KubeBackend) Starting() bool {
	kb.RLock()
	defer kb.RUnlock()
	return kb.stateStarting
}

func (kb *KubeBackend) WaitForAvailable(timeout time.Duration) error {
	if kb.Available() {
		return nil
	}
	done := make(chan struct{})
	go func() {
		kb.cond.Wait()
		close(done)
	}()
	for {
		select {
		case <-time.After(timeout):
			logrus.Warnf("waiting for backend %s available timeout", kb.ID())
			return fmt.Errorf("wait %s timeout", kb.ID())
		case <-done:
			if kb != nil && kb.Available() {
				return nil
			}
		}
	}
}
