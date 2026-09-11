package constants

// 说明：gocommon/constants 现有 ServiceNameXxx / RedisKeyPrefixXxx 仅覆盖初版服务。
// 新增 ranking-service 的常量先在本模块内定义，后续可统一提升到 gocommon
// （按本仓库约定通过发布 gocommon tag 引用，而非本地 replace）。
const (
	RedisKeyPrefixRanking = "ranking:"

	// RedisKeyBoardConfig = ranking:board:config:<board>   （RegisterBoard 持久化的展示/跳转配置）
	// RedisKeyBoardDisplay = ranking:board:display:<board>  （decorate 后的展示结果缓存）
	RedisKeyBoardConfig  = "board:config:"
	RedisKeyBoardDisplay = "board:display:"

	// LinkTemplateMemberPlaceholder 跳转模板中的占位符，渲染时替换为具体 member（文章 ID / 用户 ID）。
	// 与 board config 的 link_template（如 "/article/{member}"）约定逐字符一致，故收敛为常量。
	LinkTemplateMemberPlaceholder = "{member}"
)
