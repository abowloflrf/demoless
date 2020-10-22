package kube

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KubeBackend k8s as backend，deployment + service
type KubeBackend struct {
	sync.RWMutex
	cond          *sync.Cond
	manager       *KubeProvider
	name          string     // deployment/service name
	url           *url.URL   // parsed service address
	eps           []*url.URL // parsed endpoints address
	stateHealthy  bool       // service with at least one endpoint
	stateStarting bool       // backend is unfreezing
}

func (kb *KubeBackend) ID() string {
	return kb.name
}

func (kb *KubeBackend) Addr() *url.URL {
	// service name
	return kb.url

	// random pick from endpoints
	//if len(kb.eps) == 0 {
	//	return nil
	//}
	//return kb.eps[rand.Intn(len(kb.eps))]
}

func (kb *KubeBackend) Available() bool {
	kb.RLock()
	defer kb.RUnlock()
	return kb.stateHealthy
}

func (kb *KubeBackend) Starting() bool {
	kb.RLock()
	defer kb.RUnlock()
	return kb.stateStarting
}

// Instance Deployment replica
func (kb *KubeBackend) Instance() int {
	deployment, err := kb.manager.di.Lister().Deployments(kb.manager.namespace).Get(kb.ID())
	if err != nil {
		if err := kb.manager.syncBackend(kb.ID()); err != nil {
			logrus.Warnf("sycn app with error: %v", err)
		}
		return 0
	}
	return int(*deployment.Spec.Replicas)
}

// Unfreeze set deployment replica from 0 to 1
func (kb *KubeBackend) Unfreeze() error {
	logrus.Infof("set deployment %s replica=1", kb.ID())
	deployment, err := kb.manager.di.Lister().Deployments(kb.manager.namespace).Get(kb.ID())
	if err != nil {
		if err := kb.manager.syncBackend(kb.ID()); err != nil {
			logrus.Warnf("sycn app with error: %v", err)
		}
		return err
	}
	if *deployment.Spec.Replicas != 0 {
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

func (kb *KubeBackend) WaitForAvailable(timeout time.Duration) error {
	if kb.Available() {
		return nil
	}
	done := make(chan struct{})
	go func() {
		kb.cond.L.Lock()
		defer kb.cond.L.Unlock()
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
