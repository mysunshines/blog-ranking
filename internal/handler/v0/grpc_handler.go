package v0

import (
	"context"

	"github.com/mysunshines/blog-ranking/internal/boardconfig"
	"github.com/mysunshines/blog-ranking/internal/service"
	v0pb "github.com/mysunshines/blog-ranking/proto/pb/v0"
	pb "github.com/mysunshines/blog-ranking/proto/pb/v1"
)

// 摄入接口（RegisterBoard / RecordScore / BatchSetScore）仅由内网受信服务经 Consul
// 直连 gRPC 调用。已决定完全信任内网：不做令牌 / JWT 鉴权，仅依赖网络隔离
// （ranking 的 gRPC 端口不暴露公网），故 handler 内不再做任何调用方校验。
//
// 对外暴露为 ranking.v0.RankingService，与 C 端 v1 读接口在命名与部署上隔离。
// 公网网关 DeriveGRPCService 仅硬编码 v1（prefix.v1.PrefixService），因此本 v0 服务
// 天然不会经 Gateway 反射代理暴露，仅由内网业务服务直连（纵深防御）。

// GrpcRankingHandler gRPC 摄入接口处理器（ranking.v0.RankingService）。
type GrpcRankingHandler struct {
	v0pb.UnimplementedRankingServiceServer
	Svc service.RankingService
}

// RegisterBoard 注册榜单配置：业务方声明如何展示该 board 与是否跳转（完全信任内网，不做调用方校验）。
func (h *GrpcRankingHandler) RegisterBoard(ctx context.Context, req *pb.RegisterBoardRequest) (*pb.RegisterBoardResponse, error) {
	if req.Config == nil || req.Config.Board == "" {
		return &pb.RegisterBoardResponse{Code: 1, Message: "config.board required"}, nil
	}
	cfg := &boardconfig.BoardConfig{
		Board:            req.Config.Board,
		DecoratorType:    req.Config.DecoratorType,
		DecoratorService: req.Config.DecoratorService,
		DecoratorMethod:  req.Config.DecoratorMethod,
		LinkTemplate:     req.Config.LinkTemplate,
		CacheTTLSec:      int(req.Config.CacheTtlSec),
	}
	if err := h.Svc.RegisterBoard(ctx, cfg); err != nil {
		return &pb.RegisterBoardResponse{Code: 2, Message: err.Error()}, nil
	}
	return &pb.RegisterBoardResponse{Code: 0, Message: "success"}, nil
}

// RecordScore 内部摄入接口：推送分数变更（完全信任内网，不做调用方校验）。
func (h *GrpcRankingHandler) RecordScore(ctx context.Context, req *pb.RecordScoreRequest) (*pb.RecordScoreResponse, error) {
	if req.Board == "" || req.Member == "" {
		return &pb.RecordScoreResponse{Code: 1, Message: "board and member required"}, nil
	}
	var op int
	var delta, score float64
	switch req.Op {
	case pb.ScoreOp_SCORE_OP_INCREMENT:
		op, delta = service.OpIncrement, req.Delta
	case pb.ScoreOp_SCORE_OP_SET:
		op, score = service.OpSet, req.Score
	default:
		return &pb.RecordScoreResponse{Code: 1, Message: "invalid op"}, nil
	}
	newScore, err := h.Svc.RecordScore(ctx, req.Board, req.Member, delta, score, op)
	if err != nil {
		return &pb.RecordScoreResponse{Code: 2, Message: err.Error()}, nil
	}
	return &pb.RecordScoreResponse{Code: 0, Message: "success", Score: newScore}, nil
}

// BatchSetScore 内部摄入接口：批量覆盖设置榜单分数（历史回填，完全信任内网，不做调用方校验）。
func (h *GrpcRankingHandler) BatchSetScore(ctx context.Context, req *pb.BatchSetScoreRequest) (*pb.BatchSetScoreResponse, error) {
	if req.Board == "" {
		return &pb.BatchSetScoreResponse{Code: 1, Message: "board required"}, nil
	}
	if len(req.Items) == 0 {
		return &pb.BatchSetScoreResponse{Code: 1, Message: "items required"}, nil
	}
	items := make([]service.ScoreItem, 0, len(req.Items))
	for _, it := range req.Items {
		if it.Member == "" {
			continue
		}
		items = append(items, service.ScoreItem{Member: it.Member, Score: it.Score})
	}
	n, err := h.Svc.BatchSetScore(ctx, req.Board, items, req.PruneOthers, req.SkipNewerThan)
	if err != nil {
		return &pb.BatchSetScoreResponse{Code: 2, Message: err.Error()}, nil
	}
	return &pb.BatchSetScoreResponse{Code: 0, Message: "success", Count: n}, nil
}
