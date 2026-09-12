package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mysunshines/blog-ranking/internal/boardconfig"
	"github.com/mysunshines/blog-ranking/internal/constants"
	"github.com/mysunshines/blog-ranking/internal/decorator"
	"github.com/mysunshines/blog-ranking/internal/metrics"

	"github.com/go-redis/redis/v8"
	"github.com/mysunshines/gocommon/cache"
	"github.com/mysunshines/gocommon/log"
)

// 分数操作类型（与 proto ScoreOp 对应）
const (
	OpIncrement = 1
	OpSet       = 2
)

// GenericRankItem 业务无关的榜单条目：member 为业务 ID，display 为不透明展示结构，
// link 为跳转链接（已由模板渲染，空=不跳转）。GetRanking 统一返回此结构。
type GenericRankItem struct {
	Member  string
	Score   float64
	Rank    int64
	Display map[string]interface{}
	Link    string
}

// ScoreItem 批量摄入的单项：member 为业务 ID（如用户 ID），Score 为绝对值分数。
// BatchSetScore 用其覆盖写 ZSET，用于历史回填等一次性全量摄入。
type ScoreItem struct {
	Member string
	Score  float64
}

// RankingService 通用排行榜引擎：分数由业务方经 RecordScore / BatchSetScore 推送，
// 展示与跳转经 RegisterBoard 声明，ranking-service 自身零业务字段、不持有业务库。
type RankingService interface {
	GetRank(ctx context.Context, board, member string) (int64, float64, error)
	GetScore(ctx context.Context, board, member string) (float64, error)
	RecordScore(ctx context.Context, board, member string, delta, score float64, op int) (float64, error)
	// BatchSetScore 批量覆盖设置榜单分数（单次 ZAdd 多成员），用于历史回填。
	// pruneOthers=true 时移除 ZSET 中不在 items 内的旧成员，仅当 batch 为完整快照时开启。返回写入成员数。
	BatchSetScore(ctx context.Context, board string, items []ScoreItem, pruneOthers bool) (int64, error)

	// 统一榜单（业务无关） ----------------------------------------------------
	// RegisterBoard 注册/覆盖某 board 的展示配置（decorator + 跳转模板）。
	RegisterBoard(ctx context.Context, cfg *boardconfig.BoardConfig) error
	// GetRanking 通用取榜：按 board 配置做装饰与跳转渲染，返回业务无关条目。
	GetRanking(ctx context.Context, board string, limit int) ([]GenericRankItem, error)
}

type rankingService struct {
	boardStore *boardconfig.Store
	decoClient *decorator.Client
}

// NewRankingService 创建排行榜服务。ranking-service 不持有业务库，分数完全由外部推送。
func NewRankingService(boardStore *boardconfig.Store, decoClient *decorator.Client) RankingService {
	return &rankingService{
		boardStore: boardStore,
		decoClient: decoClient,
	}
}

// GetRank 返回 member 在 board 中的排名（1-based，0 表示未上榜）与分数。
func (s *rankingService) GetRank(ctx context.Context, board, member string) (int64, float64, error) {
	if cache.GetClient() == nil {
		return 0, 0, fmt.Errorf("redis not initialized")
	}
	rank, err := cache.ZRevRank(ctx, board, member)
	if err == redis.Nil {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	score, err := cache.ZScore(ctx, board, member)
	if err != nil {
		return 0, 0, err
	}
	return rank + 1, score, nil
}

// GetScore 返回 member 在 board 中的当前分数（未上榜返回 0）。
func (s *rankingService) GetScore(ctx context.Context, board, member string) (float64, error) {
	if cache.GetClient() == nil {
		return 0, fmt.Errorf("redis not initialized")
	}
	score, err := cache.ZScore(ctx, board, member)
	if err == redis.Nil {
		return 0, nil
	}
	return score, err
}

// RecordScore 通用分数摄入：INCREMENT 走 ZIncrBy，SET 走 ZAdd 绝对值。
// 用于积分榜/粉丝榜等「纯推送」的榜单（ranking-service 不读业务库）。
func (s *rankingService) RecordScore(ctx context.Context, board, member string, delta, score float64, op int) (float64, error) {
	if cache.GetClient() == nil {
		return 0, fmt.Errorf("redis not initialized")
	}
	switch op {
	case OpIncrement:
		return cache.ZIncrBy(ctx, board, delta, member)
	case OpSet:
		if _, err := cache.ZAdd(ctx, board, &redis.Z{Score: score, Member: member}); err != nil {
			return 0, err
		}
		return score, nil
	default:
		return 0, fmt.Errorf("unknown score op: %d", op)
	}
}

// BatchSetScore 批量覆盖设置榜单分数：单次 ZAdd 写入多个成员（原子覆盖）。
// 用于历史回填等一次性全量摄入，比逐条 RecordScore(op=SET) 少 N-1 次网络往返。
// pruneOthers=true 时移除 ZSET 中不在 items 内的旧成员（如粉丝归零的用户），
// 仅当 items 为该 board 的完整快照时开启，否则会误删其他成员。返回成功写入的成员数。
func (s *rankingService) BatchSetScore(ctx context.Context, board string, items []ScoreItem, pruneOthers bool) (int64, error) {
	if cache.GetClient() == nil {
		return 0, fmt.Errorf("redis not initialized")
	}
	zs := make([]*redis.Z, 0, len(items))
	for _, it := range items {
		if it.Member == "" {
			continue
		}
		zs = append(zs, &redis.Z{Score: it.Score, Member: it.Member})
	}
	if len(zs) == 0 {
		return 0, nil
	}
	if _, err := cache.ZAdd(ctx, board, zs...); err != nil {
		return 0, err
	}
	if pruneOthers {
		// 移除快照中已不存在的成员（与 ZAdd 后 ZRem 的清理逻辑一致）。
		old, err := cache.ZRange(ctx, board, 0, -1)
		if err != nil {
			return int64(len(zs)), err
		}
		keep := make(map[string]struct{}, len(zs))
		for _, z := range zs {
			if m, ok := z.Member.(string); ok {
				keep[m] = struct{}{}
			}
		}
		stale := make([]interface{}, 0, len(old))
		for _, m := range old {
			if _, ok := keep[m]; !ok {
				stale = append(stale, m)
			}
		}
		if len(stale) > 0 {
			if _, err := cache.ZRem(ctx, board, stale...); err != nil {
				return int64(len(zs)), err
			}
		}
	}
	return int64(len(zs)), nil
}

// RegisterBoard 注册/覆盖某 board 的展示配置（持久化于 Redis，动态生效）。
func (s *rankingService) RegisterBoard(ctx context.Context, cfg *boardconfig.BoardConfig) error {
	return s.boardStore.Set(ctx, cfg)
}

// GetRanking 统一取榜：取 TopN 的 member+score+rank，按 board 配置做装饰与跳转渲染。
// 未配置 board 时退化为「纯 member+score」（仍可用，如积分榜只需 ID 与分数）。
func (s *rankingService) GetRanking(ctx context.Context, board string, limit int) ([]GenericRankItem, error) {
	scores, err := s.zRevRange(ctx, board, limit)
	if err != nil {
		return nil, err
	}
	members := make([]string, 0, len(scores))
	for _, sc := range scores {
		if m, ok := sc.Member.(string); ok {
			members = append(members, m)
		}
	}

	var linkTmpl string
	display := map[string]map[string]interface{}{}
	c, found, err := s.boardStore.Get(ctx, board)
	switch {
	case err != nil:
		// 配置读取失败（如 Redis 不可用）：退化为纯 member+score，仅告警不影响主流程。
		log.Warnf("[ranking] load board config failed for %s: %v (degraded: no display/link)", board, err)
	case found:
		linkTmpl = c.LinkTemplate
		display = s.decorate(ctx, c, members)
	default:
		// 配置未注册。若该 board 已有分数，说明配置曾存在而后丢失（Redis 数据丢失或
		// 业务方注册失败）——必须告警：此时接口仍返回 200，前端只是"少了标题和跳转"，
		// 若不打点几乎无法发现。
		// 反之若连分数都没有，视为尚未接入的 board（如只需 ID+分数的积分榜），
		// 属预期行为，静默跳过，避免误告警。
		if len(scores) > 0 {
			metrics.RecordBoardConfigMissing(board)
		}
	}

	out := make([]GenericRankItem, 0, len(scores))
	rank := int64(1)
	for _, sc := range scores {
		m, ok := sc.Member.(string)
		if !ok {
			continue
		}
		item := GenericRankItem{Member: m, Score: sc.Score, Rank: rank}
		if d, ok := display[m]; ok {
			item.Display = d
		}
		if linkTmpl != "" {
			item.Link = strings.ReplaceAll(linkTmpl, constants.LinkTemplateMemberPlaceholder, m)
		}
		out = append(out, item)
		rank++
	}
	return out, nil
}

// decorate 按配置装饰成员：remote 回调业务服务（实现 decorator.v0.Decorator）获取展示结构；
// 未配置装饰器或类型非 remote 时返回空 display（仅 member+score）。结果按 board 缓存。
// ranking-service 对自身零业务字段，展示结构完全由业务服务定义并透传。
func (s *rankingService) decorate(ctx context.Context, cfg *boardconfig.BoardConfig, members []string) map[string]map[string]interface{} {
	cacheKey := constants.RedisKeyPrefixRanking + constants.RedisKeyBoardDisplay + cfg.Board
	if cached := s.loadDisplayCache(ctx, cacheKey); cached != nil {
		return cached
	}

	display := map[string]map[string]interface{}{}
	if cfg.DecoratorType == boardconfig.DecoRemote {
		m, err := s.decoClient.Decorate(ctx, cfg.DecoratorService, cfg.DecoratorMethod, members)
		if err != nil {
			// 下游不可用时降级：保留 member+score，仅缺展示信息，不影响榜单主流程。
			log.Warnf("[ranking] decorate remote failed for %s: %v", cfg.Board, err)
		} else {
			display = make(map[string]map[string]interface{}, len(m))
			for k, v := range m {
				display[k] = v.AsMap()
			}
		}
	}

	s.saveDisplayCache(ctx, cacheKey, display, cfg.CacheTTLSec)
	return display
}

// loadDisplayCache / saveDisplayCache 缓存「装饰结果」，降低对业务服务/DB 的读压力。
func (s *rankingService) loadDisplayCache(ctx context.Context, key string) map[string]map[string]interface{} {
	if cache.GetClient() == nil {
		return nil
	}
	raw, err := cache.Get(ctx, key)
	if err != nil || raw == "" {
		return nil
	}
	out := map[string]map[string]interface{}{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func (s *rankingService) saveDisplayCache(ctx context.Context, key string, display map[string]map[string]interface{}, ttlSec int) {
	if cache.GetClient() == nil || len(display) == 0 {
		return
	}
	b, err := json.Marshal(display)
	if err != nil {
		return
	}
	ttl := time.Duration(ttlSec) * time.Second
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	if err := cache.Set(ctx, key, string(b), ttl); err != nil {
		log.Warnf("[ranking] save display cache failed: %v", err)
	}
}

// zRevRange 取 board 的 TopN（含分数），limit 做安全裁剪。
func (s *rankingService) zRevRange(ctx context.Context, board string, limit int) ([]redis.Z, error) {
	if cache.GetClient() == nil {
		return nil, fmt.Errorf("redis not initialized")
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	return cache.ZRevRangeWithScores(ctx, board, 0, int64(limit-1))
}
