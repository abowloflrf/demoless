package docker

import (
	"context"
	"demoless/provider"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/sirupsen/logrus"
)

type DockerProvider struct {
	sync.Mutex
	client   *client.Client
	backends map[string]*DockerBackend
}

// DockerBackend docker 提供后端服务，单实例
type DockerBackend struct {
	sync.RWMutex
	name    string
	manager *client.Client
	addr    string

	// 后端实例健康，可转发请求
	stateHealthy bool
	// 容器正在启动中
	stateStarting bool
}

func (d *DockerBackend) ID() string {
	return d.name
}

func (d *DockerBackend) Addr() *url.URL {
	u, err := url.Parse(d.addr)
	if err != nil {
		logrus.Errorf("parse url err: %v", err)
		return nil
	}
	return u
}

func (d *DockerBackend) Available() bool {
	d.RLock()
	defer d.RUnlock()
	return d.stateHealthy
}

func (d *DockerBackend) Starting() bool {
	d.RLock()
	defer d.RUnlock()
	return d.stateStarting
}

func (d *DockerBackend) Instance() int {
	return 1
}

// Unfreeze 将该后端服务已停止的容器启动
func (d *DockerBackend) Unfreeze() error {
	logrus.Infof("prepare to unfreeze service: %s", d.ID())
	c, err := d.manager.ContainerInspect(context.Background(), d.ID())
	if err != nil {
		if client.IsErrNotFound(err) {
			logrus.Warnf("container not found: %v", err)
			return err
		} else {
			logrus.Errorf("inspect container err: %v", err)
			return err
		}
	}
	logrus.Debugf("Container State: %+v", c.State)
	logrus.Debugf("Container Health: %+v", *c.State.Health)
	if c.State.Running {
		// 容器已经在 Running
		d.Lock()
		d.stateStarting = false
		d.stateHealthy = c.State.Health.Status == "healthy"
		d.Unlock()
		logrus.Infof("container %s is already running, heathy status: %s", d.ID(), c.State.Health.Status)
	} else {
		logrus.Infof("start container %s", d.ID())
		// 启动容器
		err := d.manager.ContainerStart(context.Background(), d.ID(), types.ContainerStartOptions{})
		if err != nil {
			logrus.Errorf("failed to start container: %v", err)
			return err
		}
		// 修改 backend 状态
		d.Lock()
		d.stateStarting = true
		d.Unlock()
	}
	return nil
}

// WaitForAvailable 等待该后端服务的容器启动且探针通过
func (d *DockerBackend) WaitForAvailable(timeout time.Duration) error {
	logrus.Infof("start waiting for container %s running", d.ID())
	if !d.Starting() {
		return fmt.Errorf("container %s not starting", d.ID())
	}
	now := time.Now()
	for {
		select {
		case <-time.After(timeout):
			logrus.Warnf("waiting for container %s running timeout", d.ID())
			return errors.New("wait timeout")
		default:
			// check running
			c, err := d.manager.ContainerInspect(context.Background(), d.ID())
			if err != nil {
				logrus.Warnf("inspect container %s with err: %v", d.ID(), err)
				return err
			}
			logrus.Debugf("Container State: %+v", c.State)
			logrus.Debugf("Container Health: %+v", *c.State.Health)
			if c.State.Running && c.State.Health.Status == "healthy" {
				logrus.Infof("container %s is running, %v", d.ID(), time.Since(now))

				d.Lock()
				d.stateHealthy = true
				d.stateStarting = false
				d.Unlock()

				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func NewDockerProvider(host string) (provider.Provider, error) {
	var err error
	var c *client.Client
	if host != "" {
		c, err = client.NewClientWithOpts(client.WithHost(host))
	} else {
		c, err = client.NewClientWithOpts(client.FromEnv)
	}
	if err != nil {
		return nil, err
	}
	_, err = c.Ping(context.Background())
	if err != nil {
		return nil, err
	}
	dp := &DockerProvider{
		Mutex:    sync.Mutex{},
		client:   c,
		backends: make(map[string]*DockerBackend),
	}
	if err := dp.serviceDiscovery(); err != nil {
		logrus.Errorf("docker service discovery with error: %v", err)
	}
	return dp, nil
}

func (dp *DockerProvider) Find(id string) (provider.Backend, error) {
	for _, be := range dp.backends {
		if be.ID() == id {
			return be, nil
		}
	}
	return nil, fmt.Errorf("backend %s not found", id)
}

func (dp *DockerProvider) serviceDiscovery() error {
	logrus.Info("discovering service...")
	// TODO: 自动发现后端服务列表，目前写死一个
	type svc struct {
		name string
		addr string
	}
	svcs := []svc{
		{
			name: "nginx",
			addr: "http://127.0.0.1:8888",
		},
	}
	for _, s := range svcs {
		c, err := dp.client.ContainerInspect(context.Background(), s.name)
		if err != nil {
			logrus.Warnf("inspect container with err: %v", err)
			continue
		}
		if c.State.Health == nil {
			logrus.Warnf("service %s doesnot set healthy probe", s.name)
		}
		singleBackend := &DockerBackend{
			RWMutex:       sync.RWMutex{},
			name:          s.name,
			manager:       dp.client,
			addr:          s.addr,
			stateHealthy:  c.State.Health.Status == "healthy",
			stateStarting: c.State.Running && c.State.Health.Status != "healthy",
		}
		dp.backends[s.name] = singleBackend
		logrus.Infof("discovered service: %s@%s", s.name, s.addr)
	}

	// 后台循环检查服务可用性，避免容器意外退出导致状态不同步
	go func(svcs []svc) {
		for {
			time.Sleep(1 * time.Second)
			now := time.Now()
			logrus.Debugf("start docker discovery")
			backends := make(map[string]*DockerBackend)
			for _, s := range svcs {
				c, err := dp.client.ContainerInspect(context.Background(), s.name)
				if err != nil {
					logrus.Warnf("inspect container with err: %v", err)
					continue
				}
				if c.State.Health == nil {
					logrus.Warnf("service %s doesnot set healthy probe", s.name)
				}

				stateHealthy := c.State.Health.Status == "healthy"
				stateStarting := c.State.Running && c.State.Health.Status != "healthy"
				singleBackend := &DockerBackend{
					RWMutex:       sync.RWMutex{},
					name:          s.name,
					manager:       dp.client,
					addr:          s.addr,
					stateHealthy:  stateHealthy,
					stateStarting: stateStarting,
				}
				logrus.Debugf("discocvered service: %s, %+v, %+v", s.name, c.State, c.State.Health)
				backends[s.name] = singleBackend
			}
			dp.Lock()
			dp.backends = backends
			dp.Unlock()
			logrus.Debugf("complete docker discovery: %v", time.Since(now))

		}
	}(svcs)
	return nil
}
