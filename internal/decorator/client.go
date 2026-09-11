// Package decorator 榜单装饰器客户端：ranking-service 在 GetRanking 时按 board 配置
// 回调业务服务（实现 decorator.v0.Decorator），把 member 批量装饰为展示结构。
// 通过 consul 服务发现解析目标，连接复用，并经 resilience 施加超时/熔断/限流。
package decorator

import (
	"context"
	"sync"

	decoratorv0pb "github.com/mysunshines/blog-ranking/proto/decorator/v0/pb"

	"github.com/mysunshines/gocommon/consul"
	"github.com/mysunshines/gocommon/grpcclient"
	"github.com/mysunshines/gocommon/log"
	"github.com/mysunshines/gocommon/resilience"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// fullMethod 固定为 decorator.v0.Decorator/Decorate，与业务服务注册的 gRPC 方法一致。
const fullMethod = "/decorator.v0.Decorator/Decorate"

// Client 装饰器客户端，按 consul 服务名解析目标并复用连接。
type Client struct {
	disc  *consul.Discovery
	conns sync.Map // target -> *grpc.ClientConn
}

// NewClient 创建客户端。consulAddr 为 Consul Agent 地址（如 "consul:8500"）。
func NewClient(consulAddr string) *Client {
	return &Client{disc: consul.NewDiscovery(consulAddr, 0, false)}
}

func (c *Client) getConn(target string) (*grpc.ClientConn, error) {
	if v, ok := c.conns.Load(target); ok {
		return v.(*grpc.ClientConn), nil
	}
	conn, err := grpcclient.Dial(target)
	if err != nil {
		return nil, err
	}
	if actual, loaded := c.conns.LoadOrStore(target, conn); loaded {
		_ = conn.Close()
		return actual.(*grpc.ClientConn), nil
	}
	return conn, nil
}

// Decorate 回调业务服务的 Decorate，返回 member -> 展示结构（google.protobuf.Struct）。
// 失败（服务不可达/超时/熔断）返回 error，由调用方降级为「无展示信息」，不影响榜单主流程。
func (c *Client) Decorate(ctx context.Context, consulService, method string, members []string) (map[string]*structpb.Struct, error) {
	if len(members) == 0 {
		return map[string]*structpb.Struct{}, nil
	}
	target, err := c.disc.Resolve(ctx, consulService)
	if err != nil {
		return nil, err
	}
	conn, err := c.getConn(target)
	if err != nil {
		return nil, err
	}

	m := method
	if m == "" {
		m = "Decorate"
	}
	req := &decoratorv0pb.DecorateRequest{Members: members}
	resp := &decoratorv0pb.DecorateResponse{}
	invoke := func(c context.Context) error {
		return conn.Invoke(c, fullMethod, req, resp)
	}

	// 按下游服务名区分熔断/限流（与 grpcclient 的 serviceKey 语义一致）。
	policy := resilience.ForService(consulService)
	ctx = resilience.WithServiceKey(ctx, consulService)
	if err := policy.Execute(ctx, invoke, nil); err != nil {
		log.Warnf("[decorator] call %s/%s failed: %v", consulService, m, err)
		return nil, err
	}
	return resp.GetDisplay(), nil
}
