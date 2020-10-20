package main

import (
	"demoless/ingress"
	"demoless/provider"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
)

var (
	port  = pflag.IntP("port", "p", 8080, "ingress listen port")
	prov  = pflag.String("provider", "docker", "backend service provider: docker/kubernetes")
	level = pflag.IntP("level", "v", 4, "logging level 4-info 5-debug")
)

func init() {
	pflag.Parse()
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: time.StampMilli,
	})
	logrus.SetLevel(logrus.Level(*level))
}

func main() {
	// 初始化 ingress 实例
	i, err := ingress.NewIngress(*port, provider.ProviderType(*prov))
	if err != nil {
		logrus.Fatalf("create ingress proxy with err: %v", err)
	}

	// 设置优雅退出信号
	stopCh := make(chan struct{})
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-c
		logrus.Infof("gracefully shutdown: %v", s)
		close(stopCh)
		<-c
		logrus.Fatal("force exit with code 1")
	}()

	// 开启运行
	err = i.Run(stopCh)
	if err != nil {
		logrus.Fatalf("start ingress proxy with err: %v", err)
	}
}
