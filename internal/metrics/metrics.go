// Package metrics ranking-service 自有指标：gocommon/metrics 只提供各服务通用的
// 系统/gRPC/缓存指标，本服务特有的「榜单配置健康度」指标在此定义。
//
// 指标经 promauto 注册到默认 registry，与 gocommon 指标一同由 runMetricsServer 中
// 的 promhttp.Handler() 暴露（/metrics，端口 9097）。
package metrics

import (
	"sync"
	"time"

	"github.com/mysunshines/gocommon/log"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// boardConfigMissing 统计「榜单已有分数、但展示配置缺失」的 GetRanking 次数。
//
// 背景：榜单配置由业务方（article/user-service）启动时经 RegisterBoard 注册并
// 持久化于 Redis（ranking:board:config:<board>）。若 Redis 数据丢失或业务方注册
// 失败，配置就不存在，此时 GetRanking 会静默退化为「纯 member+score」——
// 前端拿不到标题/头像，也没有跳转链接，但接口仍返回 200，问题极难被发现。
//
// 判定口径（避免误告警）：仅在「ZSET 有分数、但无配置」时计数。若某 board
// 连分数都没有（ZSET 为空），说明它尚未接入（如未来的积分榜本就只需 ID+分数），
// 属预期情况，不计入。
var boardConfigMissing = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "ranking_board_config_missing_total",
		Help: "Total GetRanking calls where the board has scores but no registered config (display/link degraded)",
	},
	[]string{"board"},
)

// 日志节流状态：同一 board 的缺失告警按固定间隔输出一次，
// 避免高频读榜（默认 10s 刷新）把日志打爆。指标计数本身不受节流影响。
var (
	warnMu    sync.Mutex
	lastWarn  = make(map[string]time.Time)
	warnEvery = time.Minute
)

// RecordBoardConfigMissing 记录一次「有分数但配置缺失」的取榜，并节流输出告警日志。
// board 为榜单标识（如 board:article:views）。
func RecordBoardConfigMissing(board string) {
	boardConfigMissing.WithLabelValues(board).Inc()

	warnMu.Lock()
	defer warnMu.Unlock()
	if t, ok := lastWarn[board]; ok && time.Since(t) < warnEvery {
		return
	}
	lastWarn[board] = time.Now()
	log.Warnf("[ranking] board %s has scores but no registered config: display/link degraded; "+
		"ensure the owning service re-runs RegisterBoard (or wait for its periodic re-register)", board)
}
