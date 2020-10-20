package ingress

import (
	"demoless/provider"
	"demoless/provider/docker"
	"demoless/provider/kube"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

type IngressProxy struct {
	router     *mux.Router
	httpserver *http.Server
	port       int
	prov       provider.Provider
	stop       chan struct{}
}

func NewIngress(port int, prov provider.ProviderType) (*IngressProxy, error) {
	logrus.Infof("create ingress proxy mux router")
	var handler http.Handler
	r := mux.NewRouter()
	// 转发 header
	handler = r
	handler = handlers.ProxyHeaders(handler)
	// 记录 access log
	handler = handlers.CustomLoggingHandler(os.Stdout, handler, func(_ io.Writer, params handlers.LogFormatterParams) {
		uri := params.Request.RequestURI
		if uri == "" {
			uri = params.URL.RequestURI()
		}
		logrus.Infof("\"%s %s %s %s\" %d %d \"%s\" \"%s\"",
			params.Request.Method, params.Request.Host, uri, params.Request.Proto,
			params.StatusCode, params.Size,
			params.Request.Referer(), params.Request.UserAgent(),
		)
	})

	// 初始化 provider
	stop := make(chan struct{})
	logrus.Infof("create ingress proxy with provider: %s", prov)
	var p provider.Provider
	var err error
	switch prov {
	case provider.ProviderTypeDocker:
		p = docker.NewDockerProvider("")
	case provider.ProviderTypeKube:
		p = kube.NewKubeProvider("default")
	default:
		err = fmt.Errorf("provider %s not implement", prov)
	}
	if err != nil {
		return nil, err
	}
	ip := &IngressProxy{
		router: r,
		prov:   p,
		port:   port,
		stop:   stop,

		httpserver: &http.Server{
			Addr:         "0.0.0.0:" + strconv.Itoa(port),
			WriteTimeout: time.Second * 15,
			ReadTimeout:  time.Second * 15,
			IdleTimeout:  time.Second * 60,
			Handler:      handler,
		},
	}

	// 注册路由
	ip.registerRoutes()
	return ip, err
}

func (i *IngressProxy) registerRoutes() {
	// app route
	appRoute := i.router.Host("{app}.demoless.app")
	appRoute.HandlerFunc(i.MainHandler)

	// ingress route
	ingressRoute := i.router.Host("demoless.app").Subrouter()
	ingressRoute.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from index of demoless ingress")
	})
	ingressRoute.HandleFunc("/debug", i.DebugHandler)
}

func (i *IngressProxy) IngressIndexHandler(w http.ResponseWriter, r *http.Request) {
	RespJSON(w, R{"app": "ingress", "provider": i.prov})
}

func (i *IngressProxy) MainHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: 根据request域名判断要访问的后端服务，目前demo写死
	app := mux.Vars(r)["app"]
	logrus.Infof("request from [%s], %s", app, r.RequestURI)
	RespJSON(w, R{"app": app})
	return

	name := "nginx"
	be, err := i.prov.Find(name)
	if err != nil {
		RespErr(w, fmt.Sprintf("service not found: %v", err), http.StatusNotFound)
		return
	}
	if !be.Available() {
		logrus.Infof("request with service %s, but backend is unavailable", be.ID())
		// 且未触发解冻，即后端可用实例数为0，准备冷启动
		if !be.Starting() {
			err := be.Unfreeze()
			if err != nil {
				RespErr(w, fmt.Sprintf("unfreeze backend with err: %v", err), http.StatusBadGateway)
				return
			}
		}
		// 等待后端实例冷启动完毕
		err := be.WaitForAvailable(10 * time.Second)
		if err != nil {
			RespErr(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	proxy := httputil.NewSingleHostReverseProxy(be.Addr())
	proxy.ServeHTTP(w, r)
}

func (i *IngressProxy) DebugHandler(w http.ResponseWriter, r *http.Request) {
	RespJSON(w, R{"debug": true})
}

func (i *IngressProxy) Run(stop chan struct{}) error {
	go func() {
		if err := i.prov.Run(stop); err != nil {
			log.Fatalf("run provider with error: %v", err)
		}
	}()

	logrus.Infof("starting ingress proxy server on %s", i.httpserver.Addr)
	err := i.httpserver.ListenAndServe()
	if err != nil && errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return i.httpserver.ListenAndServe()
}
