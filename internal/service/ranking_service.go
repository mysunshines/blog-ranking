package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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
	// pruneOthers=true 时移除 ZSET 中不在 items 内的旧成员，仅当 batch 为完整快照时开启。
	// skipNewerThan 为快照时间（Unix 毫秒，回填场景必填）：>0 时开启「不回退」保护，
	// 跳过「最后更新时间晚于它」的成员并保守 prune，详见实现处注释。返回写入成员数。
	BatchSetScore(ctx context.Context, board string, items []ScoreItem, pruneOthers bool, skipNewerThan int64) (int64, error)

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
//
// 写入成功后记录该 member 的最后更新时间，供 BatchSetScore 的「不回退」保护判断。
func (s *rankingService) RecordScore(ctx context.Context, board, member string, delta, score float64, op int) (float64, error) {
	if cache.GetClient() == nil {
		return 0, fmt.Errorf("redis not initialized")
	}
	switch op {
	case OpIncrement:
		newScore, err := cache.ZIncrBy(ctx, board, delta, member)
		if err != nil {
			return 0, err
		}
		s.touchMember(ctx, board, member)
		return newScore, nil
	case OpSet:
		if _, err := cache.ZAdd(ctx, board, &redis.Z{Score: score, Member: member}); err != nil {
			return 0, err
		}
		s.touchMember(ctx, board, member)
		return score, nil
	default:
		return 0, fmt.Errorf("unknown score op: %d", op)
	}
}

// touchMember 记录 board 内某 member 的最后更新时间（Unix 毫秒）到 Redis Hash
// （ranking:board:updated:<board>，field=member，value=毫秒时间戳）。
//
// 用途：回填（BatchSetScore）时携带快照时间，跳过「比快照更新」的成员，
// 避免用旧快照覆盖掉快照之后发生的增量（否则榜单分数会回退一个回填周期）。
// best-effort：写入失败仅告警，最坏退化为"可能被覆盖"的旧行为，不影响分数写入。
func (s *rankingService) touchMember(ctx context.Context, board, member string) {
	key := constants.RedisKeyPrefixRanking + constants.RedisKeyBoardUpdated + board
	if _, err := cache.HSet(ctx, key, member, time.Now().UnixMilli()); err != nil {
		log.Warnf("[ranking] record member updated time failed: board=%s member=%s: %v", board, member, err)
	}
}

// BatchSetScore 批量覆盖设置榜单分数：单次 ZAdd 写入多个成员（原子覆盖）。
// 用于历史回填等一次性全量摄入，比逐条 RecordScore(op=SET) 少 N-1 次网络往返。
// pruneOthers=true 时移除 ZSET 中不在 items 内的旧成员（如粉丝归零的用户），
// 仅当 items 为该 board 的完整快照时开启，否则会误删其他成员。返回成功写入的成员数。
//
// skipNewerThan 是「快照时间」（Unix 毫秒，即开始读 DB 快照的时刻）。>0 时开启
// 「不回退」保护，解决全量覆盖与增量推送的竞态：
//  1. 跳过「最后更新时间 > 快照时间」的成员——它们在快照生成之后又有增量变更，
//     用旧快照覆盖会让分数回退（如快照读到浏览 100，期间 +1 变 101，回填写 100 就丢了 +1）；
//  2. prune 时同样跳过这类成员——快照之后才新增的成员（回填开始后发布的文章）
//     不在快照里，若不跳过就会被误删。
//
// 传 0 则保持原有「全量覆盖」语义（适合人工校正等确实需要强制覆盖的场景）。
func (s *rankingService) BatchSetScore(ctx context.Context, board string, items []ScoreItem, pruneOthers bool, skipNewerThan int64) (int64, error) {
	if cache.GetClient() == nil {
		return 0, fmt.Errorf("redis not initialized")
	}
	updatedKey := constants.RedisKeyPrefixRanking + constants.RedisKeyBoardUpdated + board

	// 保护模式下取「member -> 最后更新时间」全量映射。读取失败不阻断，退化为全量覆盖。
	var updated map[string]string
	if skipNewerThan > 0 {
		var err error
		updated, err = cache.HGetAll(ctx, updatedKey)
		if err != nil {
			log.Warnf("[ranking] read member updated times failed for board=%s: %v (fallback to full overwrite)", board, err)
			updated = nil
		}
	}

	// keep 记录快照中出现过的全部 member（含本轮被跳过的），prune 时据此保留，
	// 否则被跳过的成员会因为不在 zs 里而被当作"陈旧成员"删除。
	keep := make(map[string]struct{}, len(items))
	zs := make([]*redis.Z, 0, len(items))
	skipped := 0
	for _, it := range items {
		if it.Member == "" {
			continue
		}
		keep[it.Member] = struct{}{}
		if skipNewerThan > 0 && updated != nil && newerThan(updated[it.Member], skipNewerThan) {
			// 快照之后又有增量变更：保留 ZSET 中较新的值，不回退。
			skipped++
			continue
		}
		zs = append(zs, &redis.Z{Score: it.Score, Member: it.Member})
	}
	if len(zs) == 0 {
		if skipped > 0 {
			log.Infof("[ranking] backfill board=%s: skipped all %d members (updated after snapshot)", board, skipped)
		}
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
		stale := make([]string, 0)
		for _, m := range old {
			if _, ok := keep[m]; ok {
				continue
			}
			// 保守 prune：快照之后才新增/更新的成员不删（它们只是还没进快照）。
			if skipNewerThan > 0 && updated != nil && newerThan(updated[m], skipNewerThan) {
				continue
			}
			stale = append(stale, m)
		}
		if len(stale) > 0 {
			rem := make([]interface{}, 0, len(stale))
			for _, m := range stale {
				rem = append(rem, m)
			}
			if _, err := cache.ZRem(ctx, board, rem...); err != nil {
				return int64(len(zs)), err
			}
			// 同步清理更新时间记录，避免该 Hash 随成员删除而无限增长。
			if _, err := cache.HDel(ctx, updatedKey, stale...); err != nil {
				log.Warnf("[ranking] clean member updated times failed for board=%s: %v", board, err)
			}
		}
	}
	if skipped > 0 {
		log.Infof("[ranking] backfill board=%s: wrote %d, skipped %d (updated after snapshot)", board, len(zs), skipped)
	}
	return int64(len(zs)), nil
}

// newerThan 判断成员的最后更新时间是否晚于快照时间。
// 时间戳为空或解析失败时返回 false（视为"无更新记录"，按可覆盖处理）。
func newerThan(ts string, snapshotMs int64) bool {
	if ts == "" {
		return false
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	return t > snapshotMs
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
