# ranking-service 对外 API 文档

> 自动生成自 `ranking.proto`（模式：proto）。
> 网关按 `/api/v1/ranking/<snake_method>` 反射代理到 gRPC 方法 `ranking.v1.RankingService/<Method>`。
> 生成时间：2026-09-11 20:53:06
> Base URL（网关入口）：http://localhost:8080

## 接口列表

| Method | Path | 鉴权 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v1/ranking/get_ranking` | 公开 | 统一取榜：按 board 配置做装饰（member -> display）与跳转渲染，返回业务无关的结构。 |
| `GET` | `/api/v1/ranking/get_rank` | 公开 | 通用：取某 member 在 board 中的排名（1-based，0 表示未上榜）与分数 |
| `GET` | `/api/v1/ranking/get_score` | 公开 | 通用：取某 member 在 board 中的当前分数 |

## GetRanking

- **URL**: `http://localhost:8080/api/v1/ranking/get_ranking?board=<board>&limit=0`
- **Method**: `GET`
- **鉴权**: 公开（无需鉴权）

### Headers
```http
Content-Type: application/json
```

### Request
**参数位置**：Query String

| 字段 | 类型 | 说明 | 示例 |
| --- | --- | --- | --- |
| `board` | `string` | 榜单键，如 "board:user:fans" | `""` |
| `limit` | `int32` | 返回条数上限（默认 10，最大 100） | `0` |

**Query 示例**：
```json
board=<board>&limit=0
```

### Response
| 字段 | 类型 | 说明 | 示例 |
| --- | --- | --- | --- |
| `code` | `int32` | 0=成功，非 0=失败 | `0` |
| `message` | `string` | 结果描述 | `""` |
| `items` | `RankItem[]` | 榜单条目列表（含展示信息与跳转链接） | [] |

**Response 示例**：
```json
{"code": 0, "message": "success", "items": []}
```

### curl 示例
```bash
curl -X GET 'http://localhost:8080/api/v1/ranking/get_ranking?board=<board>&limit=0'
```

## GetRank

- **URL**: `http://localhost:8080/api/v1/ranking/get_rank?board=<board>&member=<member>`
- **Method**: `GET`
- **鉴权**: 公开（无需鉴权）

### Headers
```http
Content-Type: application/json
```

### Request
**参数位置**：Query String

| 字段 | 类型 | 说明 | 示例 |
| --- | --- | --- | --- |
| `board` | `string` | 榜单键，如 "board:user:fans" | `""` |
| `member` | `string` | 榜单成员 = 业务主键字符串（文章/用户 ID 转 string） | `""` |

**Query 示例**：
```json
board=<board>&member=<member>
```

### Response
| 字段 | 类型 | 说明 | 示例 |
| --- | --- | --- | --- |
| `code` | `int32` | 0=成功，非 0=失败 | `0` |
| `message` | `string` | 结果描述 | `""` |
| `rank` | `int64` | 1-based；0 表示未上榜 | `0` |
| `score` | `double` | 当前分数 | `0.0` |

**Response 示例**：
```json
{"code": 0, "message": "success", "rank": 0, "score": 0.0}
```

### curl 示例
```bash
curl -X GET 'http://localhost:8080/api/v1/ranking/get_rank?board=<board>&member=<member>'
```

## GetScore

- **URL**: `http://localhost:8080/api/v1/ranking/get_score?board=<board>&member=<member>`
- **Method**: `GET`
- **鉴权**: 公开（无需鉴权）

### Headers
```http
Content-Type: application/json
```

### Request
**参数位置**：Query String

| 字段 | 类型 | 说明 | 示例 |
| --- | --- | --- | --- |
| `board` | `string` | 榜单键，如 "board:user:fans" | `""` |
| `member` | `string` | 榜单成员 = 业务主键字符串（文章/用户 ID 转 string） | `""` |

**Query 示例**：
```json
board=<board>&member=<member>
```

### Response
| 字段 | 类型 | 说明 | 示例 |
| --- | --- | --- | --- |
| `code` | `int32` | 0=成功，非 0=失败 | `0` |
| `message` | `string` | 结果描述 | `""` |
| `score` | `double` | 当前分数 | `0.0` |

**Response 示例**：
```json
{"code": 0, "message": "success", "score": 0.0}
```

### curl 示例
```bash
curl -X GET 'http://localhost:8080/api/v1/ranking/get_score?board=<board>&member=<member>'
```

---

## 数据结构

> 下列 message / enum 被上述接口的请求或响应引用；结构体字段中的 message 类型可点击跳转到对应定义。

### RankItem

| 字段 | 类型 | 说明 | 示例 |
| --- | --- | --- | --- |
| `member` | `string` | 业务主键字符串（文章/用户 ID 转 string） | `""` |
| `score` | `double` | 该成员当前分数 | `0.0` |
| `rank` | `int64` | 1-based；0 表示未上榜 | `0` |
| `display` | `google.protobuf.Struct` | 不透明展示结构（来自 decorator，ranking 透传） | `{}` |
| `link` | `string` | 已渲染的跳转链接（空=不跳转） | `""` |

