package main

import (
	"demoless/ingress"
	"demoless/provider"
	"flag"
	"time"

	"github.com/sirupsen/logrus"
)

var (
	addr  string
	prov  string
	level int
)

func init() {
	flag.StringVar(&addr, "addr", ":8080", "ingress serve address")
	flag.StringVar(&prov, "provider", "docker", "backend service provider: docker/kubernetes")
	flag.IntVar(&level, "v", 4, "logging level 4-info 5-debug")
	flag.Parse()
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: time.StampMilli,
	})
	logrus.SetLevel(logrus.Level(level))
}

func main() {
	i, err := ingress.NewIngress(addr, provider.ProviderType(prov))
	if err != nil {
		logrus.Fatalf("create ingress proxy with err: %v", err)
	}
	err = i.Run()
	if err != nil {
		logrus.Fatalf("start ingress proxy with err: %v", err)
	}
}
