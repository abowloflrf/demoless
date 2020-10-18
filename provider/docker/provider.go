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
	client   *client.Client
	backends []provider.Backend
}

// DockerBackend docker 提供后端服务，单实例
type DockerBackend struct {
	sync.RWMutex
	identify string
	manager  *client.Client
	addr     string

	// 后端实例健康，可转发请求
	stateHealthy bool
	// 容器正在启动中
	stateStarting bool
}

func (d *DockerBackend) ID() string {
	return d.identify
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
	if c.State.Status != "running" && !d.Starting() {
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
		return fmt.Errorf("container %s not starting", d.ID)
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

func (d *DockerProvider) Find(id string) (provider.Backend, error) {
	for _, be := range d.backends {
		if be.ID() == id {
			return be, nil
		}
	}
	return nil, fmt.Errorf("backend %s not found", id)
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

	// TODO: 自动发现后端服务列表，目前写死一个
	singleBackend := &DockerBackend{
		RWMutex:       sync.RWMutex{},
		manager:       c,
		addr:          "127.0.0.1:8888",
		stateHealthy:  false,
		stateStarting: false,
	}
	return &DockerProvider{
		client:   c,
		backends: []provider.Backend{singleBackend},
	}, nil
}
