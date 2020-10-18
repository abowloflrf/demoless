package ingress

import (
	"demoless/provider"
	"demoless/provider/docker"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

type IngressProxy struct {
	router *mux.Router
	addr   string
	prov   provider.Provider
}

func NewIngress(addr string, prov provider.ProviderType) (*IngressProxy, error) {
	r := mux.NewRouter()
	// TODO: 目前只实现了docker provider
	var p provider.Provider
	var err error
	switch prov {
	case provider.ProviderTypeDocker:
		p, err = docker.NewDockerProvider("")
	case provider.ProviderTypeKube:
		err = fmt.Errorf("provider %s not implement", prov)
	default:
		err = fmt.Errorf("provider %s not implement", prov)
	}
	if err != nil {
		return nil, err
	}
	logrus.Infof("create ingress proxy with provider: %s", prov)
	ip := &IngressProxy{
		router: r,
		prov:   p,
		addr:   addr,
	}
	ip.registerRoutes()
	return ip, err
}

func (i *IngressProxy) registerRoutes() {
	i.router.HandleFunc("/", i.MainHandler)
	i.router.HandleFunc("/_debug", i.DebugHandler)
}

func (i *IngressProxy) MainHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: 根据request域名判断要访问的后端服务，目前demo写死
	name := "nginx"
	be, err := i.prov.Find(name)
	if err != nil {
		RespErr(w, fmt.Sprintf("service not found: %v", err), http.StatusNotFound)
		return
	}
	if !be.Available() {
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
	io.WriteString(w, "debug")
}

func (i *IngressProxy) Run() error {
	logrus.Infof("ingress proxy serve on: %s", i.addr)
	return http.ListenAndServe(i.addr, i.router)
}
