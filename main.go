package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/gorilla/mux"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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
	zerolog.TimeFieldFormat = time.RFC3339Nano
	log.Logger = log.Output(zerolog.ConsoleWriter{
		Out: os.Stderr,
	})
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	var err error
	dockerclient, err = client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Fatal().Msgf("error create client %v", err)
	}
}

func IndexHandler(w http.ResponseWriter, r *http.Request) {
	// log.Info().Msgf("get request")
	// defer func(start time.Time) {
	// 	log.Info().Msgf("request processed: %v", time.Since(start))
	// }(time.Now())

	if !cs.healthy {
		ngContainer, err := dockerclient.ContainerInspect(context.Background(), demoContainer)
		if err != nil {
			if client.IsErrNotFound(err) {
				// not found
				log.Fatal().Msgf("not found: %v", err)
			} else {
				log.Fatal().Msgf("get container err: %v", err)
			}
		}
		log.Debug().Msgf("State: %+v", ngContainer.State)
		log.Debug().Msgf("Health: %+v", *ngContainer.State.Health)
		if ngContainer.State.Status != "running" && !cs.starting {
			err := dockerclient.ContainerStart(context.Background(), demoContainer, types.ContainerStartOptions{})
			if err != nil {
				log.Fatal().Msgf("failed to start container: %v", err)
			}

			cs.Lock()
			cs.starting = true
			cs.Unlock()

			if err := WaitRunning(demoContainer, time.Second*10); err != nil {
				log.Fatal().Msgf("wait err: %v", err)
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
		log.Warn().Msgf("parse url err %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(backendURL)
	proxy.ServeHTTP(w, r)
}

func FooHandler(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "bar")
}

func WaitRunning(containerID string, timeout time.Duration) error {
	log.Info().Msgf("start waiting for container %s running", containerID)
	now := time.Now()
	for {
		select {
		case <-time.After(timeout):
			log.Info().Msgf("waiting for container %s running timeout", containerID)
			return errors.New("wait timeout")
		default:

			// check running
			c, err := dockerclient.ContainerInspect(context.Background(), containerID)
			if err != nil {
				log.Warn().Msgf("get container %s with err: %v", containerID, err)
				return err
			}
			log.Debug().Msgf("State: %+v", c.State)
			log.Debug().Msgf("Health: %+v", *c.State.Health)
			if c.State.Running && c.State.Health.Status == "healthy" {
				log.Info().Msgf("container %s is running, %v", containerID, time.Since(now))
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
				// not found
				log.Fatal().Msgf("not found: %v", err)
			} else {
				log.Fatal().Msgf("get container err: %v", err)
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
