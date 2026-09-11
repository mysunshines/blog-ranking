# ranking-service 排行榜服务

独立的 **gRPC 排行榜微服务**，基于 **Redis ZSET** 承载各类实时榜单，按「独立服务易扩展」原则设计：
未来新增积分榜、粉丝榜等，**无需改动核心逻辑**——只需在拥有方调用通用摄入 API 推送分数，
并通过 `RegisterBoard` 声明该榜单「怎么展示、是否跳转」（详见 §4.1 统一榜单）。

> 模块：`github.com/mysunshines/blog-ranking`（独立 Go module，遵循仓库 tag 引用约定，不依赖本地 replace）。
> 仅依赖 `github.com/mysunshines/gocommon v1.6.1`（config / cache / consul / database / middleware / observability / metrics / log）。

---

## 1. 架构核心：ZSET 实时榜 + reconcile 对账重建

排行榜本质是**有序集合（ZSET）**：`member` 为业务 ID（文章 ID / 用户 ID），`score` 为排序值（浏览数 / 点赞数 / 评论数 / 发文数）。

- **排序与实时性由 Redis ZSET 保证**：`ZREVRANGE` 取 TopN 为 O(log N + M)，天然有序、毫秒级响应。
- **榜单状态唯一来源是 ZSET**，业务表（blog 库）只作为「权威计数」的读取源，服务对其**只读**。

```
                ┌──────────────┐        周期性 reconcile（默认 30s）
   blog 库      │  articles    │ ─────────────────────────────┐
 (MySQL,只读)   │  users       │                              │
                └──────────────┘                              ▼
                                                        ┌──────────────┐
                                                        │ Redis ZSET   │  ◀── RecordScore（未来推送）
                                                        │ ranking:board*│
                                                        └──────────────┘
                                                                │ ZREVRANGE
                                                                ▼
                                                       读路径：TopN + 装饰（内置回填 / 回调业务）
```

### 为什么是 ZSET 而不是每次查 DB 排序？
- 文章/作者榜是高频读、低频变的场景。`ORDER BY view_count DESC LIMIT 10` 在大表上需全表扫描+排序；
  ZSET 把排序成本前移到写入/reconcile 阶段，读路径只需 O(log N) 取 TopN，天然支撑高并发。

---

## 2. 四个内置榜单

| 榜单 | ZSET 键（含前缀 `ranking:`） | score 来源 |
|---|---|---|
| 浏览最多文章 | `ranking:board:article:views` | `articles.view_count` |
| 点赞最多文章 | `ranking:board:article:likes` | `articles.like_count` |
| 评论最多文章 | `ranking:board:article:comments` | `articles.comment_count` |
| 发文最多作者 | `ranking:board:author:articles` | `COUNT(*) GROUP BY user_id` |

- Redis 键前缀统一为 `ranking:`（由 `constants.RedisKeyPrefixRanking` 定义；gocommon `cache.GetKey` 自动拼接）。
- 仅统计 **`status = 'published' AND deleted_at IS NULL`** 的已发布、未删除文章。

---

## 3. reconcile 机制（保证 ZSET 与 MySQL 最终一致）

`RankingService.Reconcile` 周期性（默认 30s，受环境变量 `RANKING_RECONCILE_SEC` 控制）从 blog 库
**全量**重新计算四个榜单并写入 ZSET。策略是**幂等**的「全量 ZADD + 移除已消失成员」：

```go
// 1) 读取聚合计数
views[id]   = articles.view_count
likes[id]   = articles.like_count
comments[id]= articles.comment_count
authorMap[uid] = COUNT(*)  // GROUP BY user_id

// 2) 全量覆盖 ZSET
rdb.ZAdd(key, members...)                 // 覆盖所有 member 的 score
old := rdb.ZRange(key, 0, -1)            // 取当前所有 member
// 3) 移除 MySQL 中已不存在者（文章下线/删除、作者计数归零）
stale := old - currentKeys
rdb.ZRem(key, stale...)
```

- **启动即执行一次全量**，避免冷启动榜单为空。
- **幂等、可重复执行**：多次 reconcile 结果一致；reconcile 间隔内 ZSET 与 MySQL 对齐，对排行榜场景即「实时」。
- 间隔可调小（如 5s）以获得更细的实时粒度；真正的「写入即推送」零延迟方案见 §6。

---

## 4. 读路径：ZSET 取 TopN + 一次 DB 回填

读接口从 ZSET 取 TopN（含 score），再用一次 `IN (...)` 查询回填展示字段，**避免逐条查 DB**：

- 文章榜：`LEFT JOIN users` 回填 `title / slug / cover_image / author_name`。
- 作者榜：回填 `username / avatar`。
- `limit` 在 service 层做安全裁剪（≤0 → 10，>100 → 100）。

### 4.1 统一榜单（业务无关）：展示与跳转由业务方声明

原 `GetTopArticles` / `GetTopAuthors` / `GetBoard` 属于「每业务一个方法」的写法——**每加一个榜单就要改 proto + service + handler**，
现已被 `GetRanking` 统一取代并从契约中移除。

**统一榜单**的核心：ranking-service 只负责「算名次」，**展示什么、跳不跳转全部由业务方配置驱动**，核心代码零改动。

| 概念 | 作用 |
|---|---|
| `BoardConfig` | 某 board 的展示配置：`decorator_type`（`builtin` / `remote`）、`builtin`、`decorator_service`、`decorator_method`、`link_template`、`cache_ttl_sec` |
| `Decorator`（契约 `decorator.v0.Decorator`） | **业务服务**实现 `Decorate(members) -> map<member, Struct>`，把 ID 变成用户名/头像/标题等展示信息 |
| `RankItem.display` | `google.protobuf.Struct`，**对 ranking-service 不透明**，只透传不解析 |
| `RankItem.link` | 由 `link_template`（如 `/space/{member}`）渲染；**模板为空即不跳转** |

```
GetRanking(board, limit)
  ├─ ZREVRANGE 取 TopN（member + score + rank）
  ├─ 按 BoardConfig 装饰：
  │     builtin → 本地 blog 库富化（articles / authors），不产生跨服务调用
  │     remote  → consul 解析 decorator_service，回调 decorator.v0.Decorator
  └─ 渲染 link（替换 {member} 占位符）
```

**两种装饰方式**

| 类型 | 适用场景 | 说明 |
|---|---|---|
| `builtin` | ranking-service 自己 reconcile 的榜单（文章榜、作者榜） | 复用现有 `EnrichArticles` / `EnrichAuthors` 本地富化，零网络开销 |
| `remote` | 业务拥有的榜单（粉丝榜、积分榜…） | 回调业务服务 `Decorate`，展示数据**实时来自属主服务**（用户名改了立即生效，优于本地快照） |

**设计要点**

- **新增任意榜单零改核心代码**：业务方只需做两件事——`RecordScore` 推送分数 + `RegisterBoard` 声明展示与跳转。
- 配置持久化于 Redis（`ranking:board:config:<board>`），**动态生效，无需重启/重新部署** ranking-service。
- 装饰结果按 board 缓存（`cache_ttl_sec`，默认 30s），降低对业务服务的读压力。
- **优雅降级**：remote 装饰失败（服务不可达 / 超时 / 熔断）只丢展示信息，`member / score / rank` 仍正常返回，**不影响榜单主流程**。
- 未注册的 board 退化为「纯 `member + score`」（如积分榜只需 ID 与分数），仍可正常使用。

---

## 5. gRPC 接口与网关路由

纯 gRPC 服务（gRPC 端口 9106），HTTP 8086 仅承载 `/health`、`/ready`、`/version` 探针。
本服务提供**两个 gRPC 服务**，按「对外 / 对内」分离：

- `ranking.v1.RankingService`（**对外读接口**）：开启反射，网关 `SyncFromConsul` 自动把
  `ranking-service` 推导为 `ranking.v1.RankingService`，**零改网关代码**即可暴露 `/api/v1/ranking/*`。
- `ranking.v0.RankingService`（**内部摄入接口**）：仅由内网业务服务（article/comment/user）
  经 Consul 直连 gRPC 调用，**不进入公网网关**。网关的 `DeriveGRPCService` 硬编码 `v1`，
  因此 `v0` 服务天然不会被反射代理暴露——即便令牌误配也不会被公网打到（纵深防御）。

| gRPC 方法 | 所属服务 | 网关路由 | 说明 | 鉴权 |
|---|---|---|---|---|
| `GetRank(board, member)` | `ranking.v1` | `GET /api/v1/ranking/get_rank?board=...&member=...` | 取排名（1-based，0=未上榜）与分数 | 公开 |
| `GetScore(board, member)` | `ranking.v1` | `GET /api/v1/ranking/get_score?board=...&member=...` | 取当前分数 | 公开 |
| `GetRanking(board, limit)` | `ranking.v1` | `GET /api/v1/ranking/get_ranking?board=...&limit=10` | **统一取榜**（含 `display` / `link`，业务无关） | 公开 |
| `RecordScore(...)` | `ranking.v0` | 仅内网 gRPC（不暴露公网） | 通用分数摄入 | 无（完全信任内网） |
| `BatchSetScore(...)` | `ranking.v0` | 仅内网 gRPC（不暴露公网） | 批量覆盖 / 历史回填 | 无（完全信任内网） |
| `RegisterBoard(config)` | `ranking.v0` | 仅内网 gRPC（不暴露公网） | 注册榜单展示配置（装饰器 + 跳转模板） | 无（完全信任内网） |

- 响应统一含 `code`（0 成功）、`message`、`data`。
- 三个摄入接口（`RecordScore` / `BatchSetScore` / `RegisterBoard`）均在 `ranking.v0.RankingService`，
  仅由内网服务经 Consul 直连 gRPC 调用。**已决定完全信任内网，handler 不做任何应用层鉴权**，
  安全性完全依赖网络隔离（gRPC 端口仅对内网开放、不暴露公网）。因此内网任意能连到该端口的
  负载都可读写榜单——部署时必须确保 ranking 的 gRPC 端口不挂公网 LB、不被跨租户 / 不受信网络访问。
  操作类型：`SCORE_OP_INCREMENT`（ZINCRBY）/ `SCORE_OP_SET`（ZADD 绝对值）。

---

## 6. 扩展性与「写入即推送」集成点

通用 `RecordScore(board, member, delta, op)` 已为**未来榜单（积分榜 / 粉丝榜）预留**，新榜只需在拥有方调用推送：

- 积分榜：`article/comment/user` 在用户获得积分时 `RecordScore("board:user:points", userId, +delta, INCREMENT)`。
- 粉丝榜：`user` 在 Follow/Unfollow 时 `RecordScore("board:author:followers", authorId, +1/-1, INCREMENT)`。

### 接入新榜单的完整步骤（以粉丝榜为例）

榜单要「显示用户名头像、点击跳个人空间」，需同时完成**计分**与**展示声明**：

1. **计分**：拥有方（user-service / 关注服务）在关注事件里调用
   `RecordScore("board:user:fans", userId, +1, INCREMENT)`（取关则 `-1`）。
2. **实现装饰**（一次性）：业务服务实现共享契约 `decorator.v0.Decorator`，把 member（用户 ID）转为展示信息。
   user-service 已提供参考实现（`internal/handler/v1/decorator_handler.go`，返回 `username / nickname / avatar`），
   启动时通过 `RegisterDecoratorServer` 注册到同一 gRPC Server（与 `UserService` 共存）。
3. **声明展示与跳转**（一次性）：

```json
{
  "board": "board:user:fans",
  "decorator_type": "remote",
  "decorator_service": "user-service",
  "decorator_method": "Decorate",
  "link_template": "/space/{member}",
  "cache_ttl_sec": 30
}
```

调用 `POST /api/v1/ranking/register_board`（仅管理员）写入，**即刻生效、无需重启**。

4. **前端消费**：`GET /api/v1/ranking/get_ranking?board=board:user:fans&limit=10` 返回
   `{member, score, rank, display:{username, avatar}, link:"/space/42"}`，前端直接渲染 `display` 并用 `link` 跳转。

> 全过程**无需修改 ranking-service 任何代码**。内置榜单（文章/作者）的配置在启动时由 `seedBuiltinBoards`
> 预置，`board:user:fans` 亦作为 `remote` 装饰示例预置。

### 接入「写入即推送」（零延迟实时）
当前实时性由 reconcile 间隔保证；要做到 article/comment 变更时**立刻** `ZINCRBY`，需在对应服务里
调用本服务 `RecordScore`。因 blog-ranking 需作为独立 module 发布 tag 后，才能被其它服务 `require`
（仓库既定 tag 工作流，禁止 replace），故采用「reconcile 实时 + 预留推送 API」落地。接入步骤：

1. 发布 `github.com/mysunshines/blog-ranking` 新 tag（如 `v1.0.0`）。
2. 在 `article-service` / `comment-service` 的 `go.mod` 增加 `require github.com/mysunshines/blog-ranking v1.0.0`，并 `go mod tidy`。
3. 用 `grpcclient.New` 复用 gocommon 的连接池，调用 `v0pb.NewRankingServiceClient(conn).RecordScore(...)`（注意摄入接口位于 `ranking.v0.RankingService`，仅内网可达）：
   - 文章浏览：`IncrementViewCount` flush / `PublishArticle` → `RecordScore(views, articleId, +1, INCREMENT)`
   - 文章点赞：`LikeArticle` / `CancelLike` → `RecordScore(likes, articleId, +1/-1, INCREMENT)`
   - 评论变更：`comment` 新增/删除 → `RecordScore(comments, articleId, +1/-1, INCREMENT)`
   - 以上调用失败**不影响主流程**（fire-and-forget 或带重试），榜单兜底仍由 reconcile 修正。

---

## 6.1 RecordScore 参数详解：`board` / `member` / `delta` / `op`

`RecordScore` 是接入任意新榜单的**唯一入口**，四个参数含义如下：

| 参数 | 类型 | 含义 |
|---|---|---|
| `board` | string | **榜单标识**。逻辑名形如 `board:article:views`，service 层经 `cache.GetKey` 自动拼接前缀 `ranking:` 得到最终 ZSET 键 `ranking:board:article:views`。内置四个榜单名由 `constants` 定义；扩展榜单一律由调用方约定同名字符串（如 `board:user:points`、`board:author:followers`），**保持与读路径 `GetRanking/GetRank/GetScore` 传入的 `board` 完全一致**即可。 |
| `member` | string | **榜单成员（业务主键字符串）**。即 ZSET 中的元素，必须是**唯一**的 ID 字符串：文章榜传 `strconv.FormatUint(articleId,10)`，作者/用户榜传 `strconv.FormatUint(userId,10)`。同一 `board` 内 `member` 不可重复（重复会覆盖 score）。 |
| `delta` | double | **增量值**。仅当 `op = SCORE_OP_INCREMENT` 时生效，等价于 Redis `ZINCRBY`：把该 `member` 的 score 加 `delta`（可为负，表示减分，如取消点赞 `-1`）。`SET` 模式下被忽略。 |
| `score` | double | **绝对值**。仅当 `op = SCORE_OP_SET` 时生效，等价于 Redis `ZADD`：把该 `member` 的 score **覆盖**为 `score`（适用于全量重算、断点续算或校正）。`INCREMENT` 模式下被忽略。 |
| `op` | ScoreOp 枚举 | **操作类型**：`SCORE_OP_INCREMENT = 1` → 用 `delta` 做增量；`SCORE_OP_SET = 2` → 用 `score` 设绝对值；`SCORE_OP_UNSPECIFIED(0)` / 其它 → 接口返回 `invalid op`。 |

> 约定：连续计数变更（点赞、评论、积分、粉丝）一律走 `INCREMENT` 的 `±delta`，幂等且可重试；
> 批量重算或纠正类场景（首次从 MySQL 同步、reconcile 兜底）走 `SET` 的绝对值。
> 返回 `score` 字段为操作后的最新分数，便于调用方校验。

---

## 6.2 rank 基本原理（ZSET 如何排序与排名）

榜单底层是 **Redis 有序集合（ZSET / Sorted Set）**：每个 ZSET 由一组 `(member, score)` 二元组构成，
Redis 按 `score`（浮点 double）**自动排序并去重（member 唯一）**，所有排序与排名都在写入时维护好，
读路径只要取区间，无需实时排序。

### 排序规则
- 本服务所有榜单都取「**分数高者排前**」，对应 Redis 的 `ZREVRANGE`（按 score **降序**）。
  - `GetRanking` → 内部 `zRevRange(key, 0, limit-1)` 取 TopN。
- **同分（score 相等）的兜底顺序**：Redis 在 score 相同的情况下，按 `member` 的**字典序**保持稳定排序
  （`ZREVRANGE` 下，字典序较小的 member 在前）。因此并列时名次是确定的、可复现的，不会随机抖动。

### 排名（rank）算法
- `GetRank` 调用 `ZREVRANK(key, member)`，返回的是 **0-based** 名次（第 1 名 = 0）。
  service 层将其 `+1` 转为 **1-based** 对外返回；若 member 不存在（`redis.Nil`）则返回 `0` 表示「未上榜」。
- 例：某文章 score 在 `board:article:views` 中排第 3，则返回 `rank = 3`、`score = 实际浏览数`。
- 注意：Redis 的 `ZREVRANK` 对**并列同分**的两个 member 会分别给出连续的名次（即 3、4 而非并列第 3）。
  若业务需要「并列同名次」（如都算第 3、下一名跳到第 5），需在上层按 `score` 自行归并，本服务返回的是索引名次。

### 为什么 ZSET 适合排行
| 操作 | Redis 命令 | 复杂度 | 说明 |
|---|---|---|---|
| 取 TopN | `ZREVRANGE` | O(log N + M) | M 为返回条数，毫秒级 |
| 增量更新 | `ZINCRBY` | O(log N) | 点赞/评论/浏览变化时只改一个 member |
| 设置绝对值 | `ZADD` | O(log N) | 重算 / 校正 |
| 查单个名次 | `ZREVRANK` | O(log N) | 实时看「我排第几」 |
| 查单个分数 | `ZSCORE` | O(1) | 实时看「我多少分」 |

对比 MySQL `ORDER BY view_count DESC LIMIT 10`：大表下需**全表扫描 + 排序**，随数据量增长显著变慢；
ZSET 把排序成本前移到写入 / reconcile 阶段，读路径恒定 O(log N)，天然支撑高并发排行榜。

### member 中的 score 与「实时性」的关系
- `score` 就是排名的唯一依据——它越大，名次越靠前。
- 内置榜单的 `score` 来自 blog 库计数，经 reconcile（默认 30s）刷新到 ZSET；
- 扩展榜单的 `score` 来自调用方 `RecordScore` 推送（`INCREMENT/SET`）。
- 两者写入的都只是 `ZSET`，读路径无差别，所以新增任意榜单**零改读逻辑**，这正是独立 ranking-service 易扩展的根因。

---

## 7. 端口、配置与运行

| 用途 | 端口 |
|---|---|
| gRPC（业务入口） | 9106 |
| HTTP（探针） | 8086 |
| Metrics（Prometheus 抓取） | 9097 |

- 配置：`config/config.yaml`（开发）、`config/config_test.yaml`（Docker test）、`config/config_production.yaml`（生产）。
- 关键配置项：`database`（共享 blog 库，只读）、`redis`（键前缀 `ranking:`）、`jwt.secret`、`consul.address`。
- 环境变量 `RANKING_RECONCILE_SEC` 控制对账间隔（默认 30）；`APP_ENV` 选择配置文件。
  注：`RANKING_INGEST_TOKEN` 曾用于摄入接口的受信服务令牌鉴权，现已**废弃并移除**：摄入接口改为完全信任内网、
  不做应用层鉴权，业务服务（`article/comment/user`）的 `internal/client/ranking.go` 也不再向 ranking 发送该令牌。
  部署配置中若仍保留该环境变量可直接删除。

```bash
# 本地构建运行
make build && ./bin/ranking-service

# 容器内（docker-compose 已接入 ranking-service 与 gateway depends_on、prometheus 9097 抓取）
docker compose up -d ranking-service

# 单独构建
make build-ranking          # 等价 build-ranking-service
```

---

## 8. 目录结构

```
ranking-service/
├── cmd/server/main.go          # 装配、Consul 注册、gRPC 反射、reconcile 定时循环
├── config/                     # 三套环境配置
├── proto/ranking.proto         # 对外读接口契约（源真相）：ranking.v1.RankingService
├── proto/ranking_ingest.proto   # 内部摄入接口契约：ranking.v0.RankingService（仅内网）
├── proto/decorator/v0/         # 共享装饰契约 decorator.v0.Decorator（业务服务实现，ranking 调用）
├── internal/
│   ├── constants/              # 服务名、Redis 键前缀、四个榜单键
│   ├── model/                  # 聚合计数与回填结构体（gorm tag）
│   ├── repository/             # 只读 blog 库：加载计数、IN 回填文章/作者
│   ├── boardconfig/            # 榜单展示配置存储（Redis，动态生效）
│   ├── decorator/              # 装饰客户端：consul 解析 + gRPC 回调业务服务
│   ├── service/                # 核心：reconcile 重建 ZSET + 读路径聚合 + GetRanking
│   └── handler/v1/             # gRPC 适配器（含 requireGRPCAdmin 鉴权）
├── Dockerfile / Makefile / go.mod
```
