package provider

import (
	"net/url"
	"time"
)

type ProviderType string

const (
	ProviderTypeDocker ProviderType = "docker"
	ProviderTypeKube   ProviderType = "kubernetes"
)

type Provider interface {
	Find(id string) (Backend, error)
}

type Backend interface {
	// ID 该服务的唯一标示
	ID() string
	// Addr 服务的访问地址，可以是 LB IP，Service IP 等
	Addr() *url.URL
	// Available 该服务的实例列表中至少有一个为 healthy 状态，即健康探针已过
	Available() bool
	// Instance 实例数量
	Instance() int
	// Unfreeze 执行从0到1扩容
	Unfreeze() error
	// Starting 后端实例正在启动中
	Starting() bool
	// WaitForAvailable 等待服务可用，即成功扩容到1个实例且健康探针已过
	WaitForAvailable(timeout time.Duration) error
}
