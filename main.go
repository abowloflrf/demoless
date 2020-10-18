package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

var dockerclient *client.Client
var demoContainer string

type containerState struct {
	healthy  bool
	starting bool
	sync.Mutex
}

var cs containerState

func init() {
	flag.StringVar(&demoContainer, "c", "nginx", "container name")
	flag.Parse()
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: time.StampMilli,
	})
	var err error
	dockerclient, err = client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		logrus.Fatalf("error create client: %v", err)
	}
}

func IndexHandler(w http.ResponseWriter, r *http.Request) {
	if !cs.healthy {
		c, err := dockerclient.ContainerInspect(context.Background(), demoContainer)
		if err != nil {
			if client.IsErrNotFound(err) {
				logrus.Infof("not found: %v", err)
			} else {
				logrus.Fatalf("get container err: %v", err)
			}
		}
		logrus.Debugf("Container State: %+v", c.State)
		logrus.Debugf("Container Health: %+v", *c.State.Health)
		if c.State.Status != "running" && !cs.starting {
			err := dockerclient.ContainerStart(context.Background(), demoContainer, types.ContainerStartOptions{})
			if err != nil {
				logrus.Fatalf("failed to start container: %v", err)
			}

			cs.Lock()
			cs.starting = true
			cs.Unlock()

			if err := WaitRunning(demoContainer, time.Second*10); err != nil {
				logrus.Fatalf("wait err: %v", err)
			}
		}

	}
	if cs.starting {
		for {
			time.Sleep(50 * time.Millisecond)
			if !cs.starting {
				break
			}
		}
	}

	backend := "http://127.0.0.1:8888"
	backendURL, err := url.Parse(backend)
	if err != nil {
		logrus.Warnf("parse url err: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(backendURL)
	proxy.ServeHTTP(w, r)
}

func FooHandler(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "bar")
}

func WaitRunning(containerID string, timeout time.Duration) error {
	logrus.Infof("start waiting for container %s running", containerID)
	now := time.Now()
	for {
		select {
		case <-time.After(timeout):
			logrus.Infof("waiting for container %s running timeout", containerID)
			return errors.New("wait timeout")
		default:
			// check running
			c, err := dockerclient.ContainerInspect(context.Background(), containerID)
			if err != nil {
				logrus.Warnf("get container %s with err: %v", containerID, err)
				return err
			}
			logrus.Debugf("Container State: %+v", c.State)
			logrus.Debugf("Container Health: %+v", *c.State.Health)
			if c.State.Running && c.State.Health.Status == "healthy" {
				logrus.Infof("container %s is running, %v", containerID, time.Since(now))
				cs.Lock()
				cs.healthy = true
				cs.starting = false
				cs.Unlock()
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func CheckContainerRunningLoop() {
	for {
		c, err := dockerclient.ContainerInspect(context.Background(), demoContainer)
		if err != nil {
			if client.IsErrNotFound(err) {
				logrus.Fatalf("not found: %v", err)
			} else {
				logrus.Fatalf("get container err: %v", err)
			}
		}
		if c.State.Status == "running" && c.State.Health.Status == "healthy" {
			cs.Lock()
			cs.healthy = true
			cs.starting = false
			cs.Unlock()
		} else {
			cs.Lock()
			cs.healthy = false
			cs.Unlock()
		}
		time.Sleep(1 * time.Second)
	}
}

func main() {
	go CheckContainerRunningLoop()

	r := mux.NewRouter()
	r.HandleFunc("/", IndexHandler)
	r.HandleFunc("/foo", FooHandler)
	http.ListenAndServe(":8000", r)
}
