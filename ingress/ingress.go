package ingress

import (
	"context"
	"demoless/provider"
	"demoless/provider/docker"
	"demoless/provider/kube"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

type IngressProxy struct {
	router     *mux.Router
	httpserver *http.Server
	port       int
	timout     time.Duration
	prov       provider.Provider
}

func NewIngress(port int, timeout time.Duration, prov provider.ProviderType) (*IngressProxy, error) {
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
		timout: timeout,

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
	ingressRoute.HandleFunc("/", i.IngressIndexHandler)
	ingressRoute.HandleFunc("/debug", i.DebugHandler)
	ingressRoute.Handle("/metrics", promhttp.Handler())
}

func (i *IngressProxy) MainHandler(w http.ResponseWriter, r *http.Request) {
	app := mux.Vars(r)["app"]
	be, err := i.prov.Find(app)
	if err != nil {
		RespErr(w, fmt.Sprintf("service not found: %v", err), http.StatusNotFound)
		return
	}
	time.Sleep(100 * time.Millisecond)
	if !be.Available() {
		now := time.Now()
		// 且未触发解冻，即后端可用实例数为0，准备冷启动
		if !be.Starting() {
			err := be.Unfreeze()
			if err != nil {
				RespErr(w, fmt.Sprintf("unfreeze backend with err: %v", err), http.StatusBadGateway)
				return
			}
		}
		// 等待后端实例冷启动完毕
		err := be.WaitForAvailable(i.timout)
		if err != nil {
			RespErr(w, err.Error(), http.StatusGatewayTimeout)
			return
		}
		logrus.Infof("finish waiting for service %s available, %v", app, time.Since(now))
	}
	proxy := httputil.NewSingleHostReverseProxy(be.Addr())
	proxy.ServeHTTP(w, r)
}

func (i *IngressProxy) IngressIndexHandler(w http.ResponseWriter, r *http.Request) {
	RespJSON(w, R{"app": "ingress", "provider": i.prov.Name()})
}

func (i *IngressProxy) DebugHandler(w http.ResponseWriter, r *http.Request) {
	type appinfo struct {
		Name     string
		Instance int
		Healthy  bool
		Starting bool
		URL      *url.URL
	}
	var resp []appinfo
	apps := i.prov.List()
	for _, app := range apps {
		resp = append(resp, appinfo{
			Name:     app.ID(),
			Instance: app.Instance(),
			Healthy:  app.Available(),
			Starting: app.Starting(),
			URL:      app.Addr(),
		})
	}
	RespJSON(w, R{"debug": true, "apps": resp})
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

func (i *IngressProxy) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()
	return i.httpserver.Shutdown(ctx)
}
