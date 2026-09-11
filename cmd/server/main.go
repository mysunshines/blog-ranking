package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/mysunshines/blog-ranking/internal/boardconfig"
	"github.com/mysunshines/blog-ranking/internal/constants"
	"github.com/mysunshines/blog-ranking/internal/decorator"
	v0 "github.com/mysunshines/blog-ranking/internal/handler/v0"
	v1 "github.com/mysunshines/blog-ranking/internal/handler/v1"
	"github.com/mysunshines/blog-ranking/internal/service"
	v0pb "github.com/mysunshines/blog-ranking/proto/pb/v0"
	pb "github.com/mysunshines/blog-ranking/proto/pb/v1"

	"github.com/mysunshines/gocommon/cache"
	goconfig "github.com/mysunshines/gocommon/config"
	"github.com/mysunshines/gocommon/configcenter"
	"github.com/mysunshines/gocommon/consul"
	"github.com/mysunshines/gocommon/log"
	"github.com/mysunshines/gocommon/metrics"
	"github.com/mysunshines/gocommon/middleware"
	"github.com/mysunshines/gocommon/observability"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sony/gobreaker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

// Version 由构建脚本通过 -ldflags "-X main.Version=xxx" 注入，未注入时默认 "dev"。
var Version = "dev"

// 进程级资源句柄，供 shutdown/releaseInfra 在退出时统一释放（避免多处 defer 重复释放）。
var (
	metricsCancel context.CancelFunc
	hotCfg        *configcenter.ServiceConfig
	deregister    func() error
)

type Server struct {
	cfg        *goconfig.Config
	httpServer *http.Server
	grpcServer *grpc.Server
	cb         *gobreaker.CircuitBreaker
	rankingSvc service.RankingService

	// quitCh 供内部 server goroutine 在监听失败时通知 Run 走正常关闭路径，
	// 避免 log.Fatalf 直接 os.Exit 跳过资源释放。
	quitCh chan struct{}
}

// initInfra 负责所有外部基础设施的初始化（数据库、Redis、表结构迁移）。
// 与 NewServer（纯依赖装配）分离，使 main 的启动顺序清晰可控。
// 初始化失败返回 error（由调用方统一处理，避免直接 os.Exit 导致资源泄漏）。
func initInfra(cfg *goconfig.Config) error {
	// 初始化 Redis 缓存（ZSET 榜单状态存储；失败降级，不致命）。
	redisCfg := cfg.Redis
	redisCfg.KeyPrefix = constants.RedisKeyPrefixRanking
	if err := cache.Init(&redisCfg); err != nil {
		log.Warnf("Warning: Failed to init Redis: %v", err)
	}
	return nil
}

// NewServer 仅做依赖装配（限流器/JWT/熔断器/服务/处理器），不做任何 I/O。
func NewServer(cfg *goconfig.Config, boardStore *boardconfig.Store, decoClient *decorator.Client) *Server {
	// 初始化限流器（类型别名，直接传递）
	middleware.InitRateLimiter(&cfg.RateLimit)

	// 初始化 JWT
	middleware.InitJWT(cfg.JWT.Secret)

	// 初始化熔断器
	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        cfg.App.Name,
		MaxRequests: 3,
		Interval:    10 * time.Second,
		Timeout:     30 * time.Second,
	})

	// 初始化服务层（ranking-service 不持有业务库）
	rankingSvc := service.NewRankingService(boardStore, decoClient)

	return &Server{
		cfg:        cfg,
		cb:         cb,
		rankingSvc: rankingSvc,
		quitCh:     make(chan struct{}),
	}
}

// Run 启动三组监听（HTTP 探活、gRPC、Metrics）并阻塞等待退出信号。
func (s *Server) Run() error {
	go s.runHTTPServer()
	go s.runGRPCServer()
	if goconfig.Get().Metrics.Enabled {
		go s.runMetricsServer()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-quit:
	case <-s.quitCh:
		log.Errorf("server goroutine failed, initiating shutdown")
	}

	log.Info("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.httpServer.Shutdown(ctx); err != nil {
		log.Errorf("HTTP server shutdown error: %v", err)
	}
	s.grpcServer.GracefulStop()
	log.Info("Server exited")
	return nil
}

// runHTTPServer 仅承载运维探活端点（/health、/ready、/version），不暴露任何业务路由。
func (s *Server) runHTTPServer() {
	rootMux := http.NewServeMux()
	rootMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := cache.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"unhealthy","reason":"redis"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	rootMux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	rootMux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"` + Version + `"}`))
	})

	addr := goconfig.Get().HTTP.Addr()
	h := goconfig.Get().Server.HTTP
	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           rootMux,
		ReadTimeout:       time.Duration(h.ReadTimeoutSec) * time.Second,
		ReadHeaderTimeout: time.Duration(h.ReadHeaderTimeoutSec) * time.Second,
		WriteTimeout:      time.Duration(h.WriteTimeoutSec) * time.Second,
		IdleTimeout:       time.Duration(h.IdleTimeoutSec) * time.Second,
	}

	log.Infof("HTTP server (probe-only) starting on %s", addr)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Errorf("Failed to start HTTP server: %v", err)
		close(s.quitCh)
	}
}

// runGRPCServer 运行 gRPC 服务器（本服务唯一的业务入口）。
func (s *Server) runGRPCServer() {
	lis, err := net.Listen("tcp", goconfig.Get().GRPC.Addr())
	if err != nil {
		log.Errorf("Failed to listen: %v", err)
		close(s.quitCh)
		return
	}

	g := goconfig.Get().Server.GRPC
	grpcOpts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     time.Duration(g.MaxConnectionIdle) * time.Second,
			MaxConnectionAge:      time.Duration(g.MaxConnectionAge) * time.Second,
			MaxConnectionAgeGrace: time.Duration(g.MaxConnectionAgeGrace) * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             time.Duration(g.MinPingInterval) * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.MaxConcurrentStreams(g.MaxConcurrentStreams),
		grpc.ChainUnaryInterceptor(
			middleware.GRPCRecoveryInterceptor(s.cfg.App.Name),
			middleware.GRPCTimeoutInterceptor(s.cfg.App.Name),
			middleware.GRPCCircuitBreakerInterceptor(s.cb),
			middleware.GRPCAuthInterceptor(),
			middleware.GRPCMetricsInterceptor(s.cfg.App.Name),
			middleware.GRPCLoggingInterceptor(),
		),
	}
	grpcOpts = append(grpcOpts, observability.GRPCServerOptions()...)

	s.grpcServer = grpc.NewServer(grpcOpts...)
	// 注册业务服务 + 开启 gRPC 反射（Gateway 经反射动态发现 /api/v1/ranking/* 路由）
	pb.RegisterRankingServiceServer(s.grpcServer, &v1.GrpcRankingHandler{Svc: s.rankingSvc})
	v0pb.RegisterRankingServiceServer(s.grpcServer, &v0.GrpcRankingHandler{Svc: s.rankingSvc})
	reflection.Register(s.grpcServer)

	log.Infof("gRPC server starting on %s", goconfig.Get().GRPC.Addr())
	if err := s.grpcServer.Serve(lis); err != nil && err != grpc.ErrServerStopped {
		log.Errorf("Failed to serve gRPC: %v", err)
		close(s.quitCh)
	}
}

func (s *Server) runMetricsServer() {
	addr := fmt.Sprintf(":%d", goconfig.Get().Metrics.Port)
	http.Handle(goconfig.Get().Metrics.Path, promhttp.Handler())
	log.Infof("Metrics server starting on %s%s", addr, goconfig.Get().Metrics.Path)
	if err := http.ListenAndServe(addr, nil); err != nil && err != http.ErrServerClosed {
		log.Errorf("Metrics server error: %v", err)
	}
}

func main() {
	var runErr error
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("panic recovered in main: %v\n%s", r, debug.Stack())
			runErr = fmt.Errorf("panic: %v", r)
		}
		if runErr != nil {
			log.Errorf("%s exited: %v", "ranking-service", runErr)
		}
		releaseInfra()
		if runErr != nil {
			os.Exit(1)
		}
	}()

	runErr = run()
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	log.Init(cfg.App.LogDir, cfg.App.LogLevel, cfg.App.Name)
	log.EnableLokiFromConfig(cfg.Loki, cfg.App.Name)
	observability.InitAndRegister(cfg.App.Name, cfg.OTel)

	metrics.Init(cfg.App.Name)
	metricsCtx, metricsCancelFn := context.WithCancel(context.Background())
	metricsCancel = metricsCancelFn
	metrics.StartRuntimeMetrics(metricsCtx, 15*time.Second)
	metrics.StartHealthReporter(metricsCtx, cfg.App.Name, 10*time.Second, nil, cache.Ping)

	// 配置中心热更（限流阈值/日志级别等）；缺失时降级到默认值。
	hotCfg = configcenter.Init(cfg.Consul.Address, cfg.App.Name, cfg.App.Env)
	if err := hotCfg.Load(); err != nil && err != configcenter.ErrNotFound {
		log.Warnf("load hot config failed: %v", err)
	}
	go hotCfg.Watch()

	if err := initInfra(cfg); err != nil {
		return err
	}

	consul.UseConsulDiscovery(cfg.Consul.Address)

	// 统一榜单依赖：配置存储 + 装饰器客户端（回调业务服务实现 decorator.v0.Decorator）。
	boardStore := boardconfig.NewStore()
	decoClient := decorator.NewClient(cfg.Consul.Address)

	deregister, err = registerToConsul(cfg)
	if err != nil {
		return err
	}

	server := NewServer(cfg, boardStore, decoClient)

	if err = server.Run(); err != nil {
		return fmt.Errorf("server error: %v", err)
	}

	shutdown()
	return nil
}

func shutdown() {
	if deregister != nil {
		if err := deregister(); err != nil {
			log.Warnf("consul deregister: %v", err)
		}
	}
	releaseInfra()
}

func releaseInfra() {
	if hotCfg != nil {
		hotCfg.Stop()
	}
	if metricsCancel != nil {
		metricsCancel()
	}
	if err := cache.Close(); err != nil {
		log.Warnf("cache close: %v", err)
	}
	observability.ShutdownGlobal(context.Background())
	log.StopRotation()
}

func loadConfig() (*goconfig.Config, error) {
	cfg, err := goconfig.LoadByEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %v", err)
	}
	return cfg, nil
}

func registerToConsul(cfg *goconfig.Config) (func() error, error) {
	deregister, err := consul.Register(consul.Registration{
		Name:               cfg.App.Name,
		ConsulAddress:      cfg.Consul.Address,
		GRPCPort:           cfg.GRPC.Port,
		HTTPPort:           cfg.HTTP.Port,
		CheckInterval:      cfg.Consul.CheckInterval,
		DeregisterCritical: cfg.Consul.DeregisterCritical,
		Version:            consul.VersionFromEnv(Version),
		Canary:             consul.CanaryFromEnv(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to register to consul: %v", err)
	}
	return deregister, nil
}
