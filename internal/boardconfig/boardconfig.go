// Package boardconfig 榜单配置存储：业务方通过 RegisterBoard 声明某 board 如何展示
// （decorator）与是否跳转（link_template），ranking-service 仅按配置行事，新增任何
// 榜单都无需改动核心代码。配置以 JSON 持久化于 Redis，动态生效、无需重启。
package boardconfig

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mysunshines/blog-ranking/internal/constants"

	"github.com/go-redis/redis/v8"
	"github.com/mysunshines/gocommon/cache"
	"github.com/mysunshines/gocommon/log"
)

// 装饰器类型
const (
	// DecoRemote 回调业务服务（实现 decorator.v0.Decorator），ranking-service 不感知业务字段。
	DecoRemote = "remote"
)

// BoardConfig 榜单配置，与 proto BoardConfig 一一对应，JSON 存储于 Redis。
type BoardConfig struct {
	Board            string `json:"board"`
	DecoratorType    string `json:"decorator_type"`    // remote
	DecoratorService string `json:"decorator_service"` // remote: consul 服务名，如 user-service
	DecoratorMethod  string `json:"decorator_method"`  // remote: 方法名，默认 Decorate
	LinkTemplate     string `json:"link_template"`     // 跳转模板，如 /space/{member}；空=不跳转
	CacheTTLSec      int    `json:"cache_ttl_sec"`     // 富化结果缓存秒数，<=0 用默认
}

func (c *BoardConfig) configKey() string {
	return constants.RedisKeyPrefixRanking + constants.RedisKeyBoardConfig + c.Board
}

// Store 榜单配置存储（Redis 支撑）。
type Store struct{}

// NewStore 创建配置存储。
func NewStore() *Store { return &Store{} }

// Set 写入/覆盖某 board 配置（持久化，不过期；可由 RegisterBoard 重新下发）。
func (s *Store) Set(ctx context.Context, cfg *BoardConfig) error {
	if cfg.Board == "" {
		return fmt.Errorf("board required")
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := cache.Set(ctx, cfg.configKey(), string(b), 0); err != nil {
		return err
	}
	log.Infof("[boardconfig] registered board=%s type=%s", cfg.Board, cfg.DecoratorType)
	return nil
}

// Get 读取某 board 配置。未配置时 found=false，调用方按「纯 member+score」处理。
func (s *Store) Get(ctx context.Context, board string) (*BoardConfig, bool, error) {
	if cache.GetClient() == nil {
		return nil, false, fmt.Errorf("redis not initialized")
	}
	raw, err := cache.Get(ctx, constants.RedisKeyPrefixRanking+constants.RedisKeyBoardConfig+board)
	if err != nil {
		if errorsIsNil(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if raw == "" {
		return nil, false, nil
	}
	cfg := &BoardConfig{}
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		log.Warnf("[boardconfig] parse failed for %s: %v", board, err)
		return nil, false, nil
	}
	return cfg, true, nil
}

func errorsIsNil(err error) bool {
	return err == redis.Nil
}
