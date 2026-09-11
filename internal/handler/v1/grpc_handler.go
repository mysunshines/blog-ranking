package v1

import (
	"context"

	"github.com/mysunshines/blog-ranking/internal/service"
	pb "github.com/mysunshines/blog-ranking/proto/pb/v1"

	"google.golang.org/protobuf/types/known/structpb"
)

// 读接口（GetRank / GetScore / GetRanking）对外暴露为 ranking.v1.RankingService，
// 经公网网关（Gateway）反射代理暴露给 C 端，属于读路径、无写入能力。
// 摄入（写入）逻辑已拆分到 internal/handler/v0（ranking.v0.RankingService），
// 仅由内网业务服务经 Consul 直连，与 C 端读接口在命名与部署上隔离。

// GrpcRankingHandler gRPC 排行榜读接口处理器（ranking.v1.RankingService）。
type GrpcRankingHandler struct {
	pb.UnimplementedRankingServiceServer
	Svc service.RankingService
}

// GetRank 取某成员排名与分数
func (h *GrpcRankingHandler) GetRank(ctx context.Context, req *pb.GetRankRequest) (*pb.GetRankResponse, error) {
	if req.Board == "" || req.Member == "" {
		return &pb.GetRankResponse{Code: 1, Message: "board and member required"}, nil
	}
	rank, score, err := h.Svc.GetRank(ctx, req.Board, req.Member)
	if err != nil {
		return &pb.GetRankResponse{Code: 2, Message: err.Error()}, nil
	}
	return &pb.GetRankResponse{Code: 0, Message: "success", Rank: rank, Score: score}, nil
}

// GetScore 取某成员当前分数
func (h *GrpcRankingHandler) GetScore(ctx context.Context, req *pb.GetScoreRequest) (*pb.GetScoreResponse, error) {
	if req.Board == "" || req.Member == "" {
		return &pb.GetScoreResponse{Code: 1, Message: "board and member required"}, nil
	}
	score, err := h.Svc.GetScore(ctx, req.Board, req.Member)
	if err != nil {
		return &pb.GetScoreResponse{Code: 2, Message: err.Error()}, nil
	}
	return &pb.GetScoreResponse{Code: 0, Message: "success", Score: score}, nil
}

// GetRanking 通用取榜（业务无关）：按 board 配置做装饰与跳转渲染。
func (h *GrpcRankingHandler) GetRanking(ctx context.Context, req *pb.GetRankingRequest) (*pb.GetRankingResponse, error) {
	if req.Board == "" {
		return &pb.GetRankingResponse{Code: 1, Message: "board required"}, nil
	}
	items, err := h.Svc.GetRanking(ctx, req.Board, int(req.Limit))
	if err != nil {
		return &pb.GetRankingResponse{Code: 2, Message: err.Error()}, nil
	}
	out := make([]*pb.RankItem, 0, len(items))
	for _, it := range items {
		ri := &pb.RankItem{
			Member: it.Member,
			Score:  it.Score,
			Rank:   it.Rank,
			Link:   it.Link,
		}
		if len(it.Display) > 0 {
			if s, err := structpb.NewStruct(it.Display); err == nil {
				ri.Display = s
			}
		}
		out = append(out, ri)
	}
	return &pb.GetRankingResponse{Code: 0, Message: "success", Items: out}, nil
}
