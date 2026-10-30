# CONTRACT · ElevenLabs 适配器对外契约

**版本**：v1（2026-10-26）
**适用**：Python 版 v0.4.0 与 Go 版（现状实现）**必须同时满足**这份文档。
**用途**：Go 版重写的唯一验收依据。任何一处不一致，要么改代码，要么改文档并写清理由 —— 不允许「实现完再倒推契约」。

---

## 0. 关于「计费对齐」的一条事实更正

Python 版 README 写的是「回传 `X-Usage-Input-Characters`，让中转站按字符而不是 tokenizer 估算扣费」。
**这句话与 new-api 的实际行为不符**，Go 版以本节为准：

```
relaykit/dto/audio.go            GetTokenCountMeta() -> CombineText: r.Input
service/token_counter.go         utf8.RuneCountInString(meta.CombineText)
relay/channel/openai/audio.go    usage.PromptTokens = info.GetEstimatePromptTokens()
```

new-api **自己数**字符，不读适配器的任何 `X-Usage-*` 头。
所以：

| 命题 | 结论 |
|---|---|
| 适配器能让 new-api 按字符计费吗 | **不能**。改适配器永远做不到，要改 new-api（billingexpr） |
| 那 `X-Usage-*` 有什么用 | 自证 + 对账锚点，让人能逐笔核对「提交多少 / 合成多少 / 上游扣多少」 |
| 账本数字该跟谁对齐 | `utf8.RuneCountInString(input)`，不是「与 Python 版相等」 |

---

## 1. 端点清单

| 方法 | 路径 | 状态 | 说明 |
|---|---|---|---|
| POST | `/v1/audio/speech` | ✅ 已实现 | 文本转语音（非流式） |
| GET | `/v1/models` | ✅ | OpenAI 格式模型列表，默认只含标准引擎 |
| GET | `/v1/presets` | ✅ | 补充包清单与启用状态 |
| GET | `/v1/voices` | ✅ | 音色目录，`with_preview=1` 带试听链接 |
| GET | `/v1/usage` | ✅ | 用量看板（只读本地账本） |
| GET | `/healthz` | ✅ | 健康检查，**豁免 IP 白名单** |
| GET | `/` | ✅ | 静态页（试听 + 配置生成器） |
| GET | `/static/*` | ✅ | 静态资源 |
| POST | `/v1/audio/speech/jobs` | ✅ | 提交长文本任务，202 + `Location` |
| GET | `/v1/audio/speech/jobs` | ✅ | 任务列表（运维视角，最近 50 条） |
| GET | `/v1/audio/speech/jobs/{id}` | ✅ | 轮询状态 |
| GET | `/v1/audio/speech/jobs/{id}/audio` | ✅ | 下载音频，**不检查开关** |
| DELETE | `/v1/audio/speech/jobs/{id}` | ✅ | 取消排队中/进行中的任务 |
| GET | `/v1/demo/config` | ✅ | 试听配置（开没开、剩几次、可选音色） |
| POST | `/v1/demo/speech` | ✅ | 免注册试听，服务端 Key，四道闸门 |

---

## 2. `POST /v1/audio/speech` 请求字段

| 字段 | 类型 | 必填 | 默认 | 约束 |
|---|---|---|---|---|
| `model` | string | 否 | `eleven_multilingual_v2` | 引擎名 / 别名 / OpenAI 兼容名 |
| `input` | string | **是** | — | 非空；清理后为空则 400 |
| `voice` | string \| null | 否 | null | 音色名或 10–64 位 `voice_id` |
| `response_format` | string | 否 | `mp3` | `mp3`/`opus`/`wav`/`pcm`；`aac`/`flac` 显式 400 |
| `speed` | number \| numeric-string | 否 | null | `0.25–4.0`，超范围 400；下发前钳制到 `0.7–1.2` |
| `stream` | bool \| "true"/"false" \| 0/1 | 否 | false | `true` → 501 |
| `text_clean` | string | 否 | 服务端配置 | `off`/`safe`/`normalize`，**只能加强不能削弱** |

**宽松类型（pydantic lax 兼容，Go 版必须实现）**：`speed` 传字符串 `"1.0"`、`stream` 传 `"true"` 或 `1`，
Python 版都接受。Go 的 `encoding/json` 默认会报错 —— 直接照着写就是一次全量客户故障。

**未知字段**：忽略，不报错（两侧一致）。

---

## 3. 响应头（成功路径）

| 头 | 触发条件 | 值 |
|---|---|---|
| `X-Request-Id` | 总是（**失败也有**） | 16 位十六进制 |
| `X-Usage-Input-Characters` | 总是 | 计费字符数（`bill_chars`） |
| `X-Usage-Source` | 总是 | `input` / `chars` / `upstream` / `failed` |
| `X-Usage-Rule` | 总是 | **纯 ASCII** 计费依据，例 `status=200 mode=input bill=input=9` |
| `X-Usage-Upstream-Credits` | 上游回传 `character-cost` 时 | 上游真实积分 |
| `X-ElevenLabs-Model` | 总是 | 实际下发的引擎名 |
| `X-ElevenLabs-Voice` | 总是 | 实际下发的 voice_id |
| `X-Text-Clean-Mode` | 总是 | 本次生效档位 |
| `X-Text-Clean` | `removed > 0` | `N chars removed by a,b,c` |
| `X-Text-Raw-Chars` | `removed > 0` | 原始字符数 |
| `X-Text-Sent-Chars` | `removed > 0` | 实际合成字符数 |
| `X-Text-Saved-Chars` | `removed > 0` | `raw − sent`（≥0） |
| `X-Speed-Clamped` | 发生钳制 | 钳制后的值，例 `1.2` |
| `X-Mock` / `X-Mock-Format` / `X-Mock-Format-Mismatch` | MOCK 模式 | 见 §6 |

**硬约束：头值只能是 latin-1。**
中文写进 HTTP 头会让**整个响应**崩掉（不是只丢那个头）。所以「账本用的中文说明」与
「响应头用的 ASCII 说明」必须是两个函数（Go：`billing.RuleText` / `billing.RuleHeader`）。

**大小写保真**：`X-ElevenLabs-*` 的驼峰必须原样输出。Go 的 `http.Header.Set` 会 canonical 化成
`X-Elevenlabs-*`；虽不违反 HTTP 规范，但会让 M6 的逐头 diff 产生 22 条假差异。
Go 版直接写底层 map 绕过。

---

## 4. 错误契约

结构（OpenAI 兼容）：

```json
{"error":{"message":"...","type":"invalid_request_error","code":"invalid_request_error","fix":"下一步改哪一行"}}
```

`fix` 是纯增量字段：老客户端只读 `message`，不受影响。

**上游状态码映射**

| 上游 | 下游 | 理由 |
|---|---|---|
| `401` | **401** | 原样保留：面板据此判定渠道密钥失效并自动禁用渠道，这是期望行为 |
| `402` | 403 | 上游语义是「额度不足」，对下游按权限拒绝更贴切 |
| `403` | 403 | — |
| `404` | 404 | — |
| `422` | 400 | 上游参数校验失败 = 请求方的问题 |
| `429` | 429 | — |
| `5xx` | 502 | — |
| 其它 | 400 | — |

**适配器自身状态码**：`400` 参数/校验、`401` 缺 Key、`403` IP 白名单、`413` 请求体超限、
`429` 并发满、`501` stream=true、`502` 上游网络/崩溃。

**异步任务专用**：`202` 已受理、`404` 未开启 / job_id 不存在 / 已过期、`409` 任务未完成或已终态
（`error.code` 一律 `invalid_request_error`，队列满才是 `rate_limit_error`，与 Python 版逐字一致）。
`/v1/audio/speech/jobs/{id}/audio` **不检查** `ADAPTER_JOBS_ENABLED` —— 开关只控制能不能提交新任务，
不该让已完成的结果突然取不到。

**失败请求也必须带 `X-Request-Id`** —— 客户说「这次失败还被扣钱了」时，没有 rid 就无从查起。

---

## 5. 计费账本（JSONL，按天切分）

路径：`ADAPTER_BILLING_LOG_DIR/YYYY-MM-DD.jsonl`，一行一个请求，UTF-8 不转义。

| 字段 | 类型 | 含义 |
|---|---|---|
| `ts` | int | Unix 秒 |
| `rid` | string | 请求 id（异步任务为 `job:<id>`） |
| `up_rid` | string \| null | 上游 request-id |
| `model` | string \| null | 显式传 voice 时记 voice |
| `voice` | string \| null | 解析后的 voice_id |
| `in_chars` | int | **客户提交的原始长度（不 strip）** = 计费依据 |
| `sent_chars` | int | **实际送上游合成的长度** = 成本依据 |
| `bill_chars` | int | 向客户收费的数量（按 `ADAPTER_BILLING_MODE`） |
| `bill_source` | string | `input` / `chars` / `upstream` / `failed` |
| `bill_rule` | string | 中文计费依据（账本可中文，头不行） |
| `up_credits` | int \| null | 上游真实扣的积分，拿不到为 null（**绝不估算顶替**） |
| `status` | int | 下游状态码 |
| `ms` | int | 耗时毫秒 |
| `mode` | string | 写入时的计费口径 |
| `ip` | string \| null | TCP 对端地址 |
| `token` | string \| null | 密钥前 6 位 + `…`，**不落完整密钥** |
| `error` | string | 仅失败行，截断 200 字符 |
| `raw_chars` / `cleaned_chars` / `clean_rules` | int/int/string\|null | 清理明细 |
| `async` | bool | 仅异步任务行 |

**三个数缺一不可**：`in_chars`（收钱）、`sent_chars`（我方成本）、`up_credits`（上游真实扣费）。
只留一个数，毛利就算不准。

**口径铁律**

| `ADAPTER_BILLING_MODE` | `bill_chars` | `saved_chars` 的含义 |
|---|---|---|
| `input`（默认） | `in_chars`，与 new-api 逐笔相等 | **上游成本节省**，不是计费折扣（客户仍按 raw 付费） |
| `chars` | `sent_chars` | 真正的「没向客户收的部分」 |
| `upstream` | `up_credits`，拿不到则退回 `in_chars` | 同 input |

**失败请求 `bill_chars` 恒为 0**：new-api 对非 200 不扣费，账本若仍计费，两边永远对不上。

**异步任务也写同一本账**：`rid = job:<id>`、`async = true`。四类结局的 `status`：

| 结局 | 账本 `status` | 说明 |
|---|---|---|
| 成功 | `200` | 正常计费 |
| 上游报错 | **上游原始状态码**（422/429/500…） | 与 Python 版一致，不是映射后的下游码 |
| 客户取消（在途中） | `499` | 上游可能已扣费，必须留痕；`bill_chars=0` |
| 崩溃/关闭中断 | `502` | 同上 |
| 本地校验失败（voice/format） | **不写行** | 压根没碰上游，不制造噪声；与 Python 版一致 |

**试听（demo）刻意不记账**：它用的是服务端自己的 Key，没有 new-api 侧的对应记录；
混进账本会让 L1 对账凭空多出一批「有计费无日志」的行，把真正的漏记淹掉。

---

## 6. 配置项（26 项，名字与默认值不得改）

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `ELEVENLABS_API_BASE` | `https://api.elevenlabs.io/v1` | 上游地址 |
| `ELEVENLABS_API_KEY` | 空 | 兜底 Key（主路径是请求头透传） |
| `ELEVENLABS_MOCK` | `0` | `1` = 返回静音 WAV，不消耗额度 |
| `ELEVENLABS_VOICES_FILE` | 空 | 外部音色表（Go 版内置一份，此项覆盖） |
| `ELEVENLABS_DEFAULT_MODEL` | `eleven_multilingual_v2` | — |
| `ELEVENLABS_TIMEOUT` | `300` | 秒；实测约 33ms/字 |
| `ELEVENLABS_MAX_RETRIES` | `0` | 仅 5xx/429/连接中断重试；**超时永不重试** |
| `ADAPTER_HOST` / `ADAPTER_PORT` | `127.0.0.1` / `8123` | — |
| `ADAPTER_MAX_CHARS` | `0` | 单次字符上限，0 不限（按 sent 判断） |
| `ADAPTER_MAX_BODY_BYTES` | `2097152` | 请求体上限，含 chunked |
| `ADAPTER_MAX_CONCURRENCY` | `0` | 进程内并发上限，超出 429 |
| `ADAPTER_ALLOW_IPS` | 空 | **TCP 对端地址**，不读 XFF |
| `ADAPTER_ALLOW_LOCAL_KEY` | `0` | 允许回落 `~/.elevenlabs/config.json` |
| `ADAPTER_PRESETS` | `none` | `all` / `none` / `compat,en,cn,tw` |
| `ADAPTER_JOBS_ENABLED` | `0` | 异步任务总开关 |
| `ADAPTER_JOBS_WORKERS` | `2` | 并发 worker 数（= 上游并发上限） |
| `ADAPTER_JOBS_MAX_PENDING` | `50` | 排队上限，超出 429；`<=0` 视为不限 |
| `ADAPTER_JOBS_MAX_CHARS` | `8000` | 单任务字符上限（按清理后长度判断） |
| `ADAPTER_JOBS_TTL_SECONDS` | `1800` | 结果保留时长，到期删元数据与音频 |
| `ADAPTER_JOBS_DIR` | `logs/jobs` | **Go 版新增**：任务表目录（`<id>.json` + `<id>.audio`） |
| `ADAPTER_DEMO_ENABLED` | `0` | 试听总开关 |
| `ADAPTER_DEMO_MAX_CHARS` | `20` | 单次试听字符上限（超出截断） |
| `ADAPTER_DEMO_PER_HOUR` | `20` | 每 IP 每小时次数 |
| `ADAPTER_DEMO_VOICES` | `zh-yingshi,zh-boyin,zh-pangbai` | 白名单音色，解析为空时回落默认并告警 |
| `ADAPTER_DEMO_TRUST_XFF` | `0` | **Go 版新增**：信任 `X-Forwarded-For`（反代后必须开，否则限额全站共享） |
| `ADAPTER_BILLING_MODE` | `input` | `input`/`chars`/`upstream` |
| `ADAPTER_BILLING_LOG_DIR` | `logs/billing` | — |
| `ADAPTER_BILLING_LOG_DAYS` | `30` | — |
| `ADAPTER_BILLING_LOG_OFF` | `0` | 关闭账本（不建议） |
| `ADAPTER_UP_CREDITS_PER_USD` | `0` | 0 = 不算毛利率（宁可 null，不给错数） |
| `ADAPTER_CHARS_PER_USD` | `0` | 同上 |
| `ADAPTER_TEXT_CLEAN` | `safe` | `off`/`safe`/`normalize` |
| `ADAPTER_STATIC_DIR` | 空 | **Go 版新增**：外部静态目录覆盖（便于改页面不重编） |

---

## 7. 已知且**有意**的差异（Go vs Python）

| # | 差异 | 影响 | 决策 |
|---|---|---|---|
| D1 | Markdown 清理规则由 9 条拆成 11 条 | RE2 不支持反向引用 `\1` | 语义等价，已用 12 条对照用例钉住 |
| D2 | `\s` 的字符集合：Python 含 Unicode 空白（如 U+3000），Go 只含 `[\t\n\f\r ]` | `normalize` 档下以全角空格开头的标题处理不同 | 接受。`safe` 档显式列了 `\u3000`，不受影响 |
| D3 | 校验错误文案措辞与 pydantic 不完全一致（结构与状态码一致） | 客户若硬编码文案会看到差异 | 接受。`code`/`type`/HTTP 状态码 100% 一致 |
| D4 | 请求体 JSON 键顺序（Go map 按字母序，Python 按插入序） | 仅影响抓包可读性，不影响上游语义 | 接受 |
| D5 | `/v1/voices` 的 `by_token` 同值时按 token 升序（Python 按插入序） | 输出更可复现 | 接受（改进） |
| D6 | Go 版静态页与 `voices.json` 打进二进制 | 部署更简单；改它们要重编或有 `ADAPTER_STATIC_DIR` | 改进 |
| D7 | Go 版账本文件句柄按天缓存复用 | 少一次 syscall；语义不变 | 改进 |
| D8 | ~~jobs / demo 端点未迁移~~ | 已在 M4/M5 补齐 | 已完成 |
| D9 | 任务的 `chars` 用**原始长度**（不 strip）；Python 用 `count_chars`（strip 过） | 同一任务在「详情」与「账本」上，Python 会显示两个不同的字符数 | 采用 Go 语义。M6 对账时按 D9 白名单放行 |
| D10 | 任务失败时多出 `error_code` / `never_started` | 纯增量字段，老客户端按 `status=="failed"` 判断不受影响 | 改进：能区分「客户写错」与「服务端出事」 |
| D11 | 任务表落盘到 `ADAPTER_JOBS_DIR`；重启后中断任务转 `failed` 而非消失 | Python 重启后轮询直接 404 | 改进。**刻意不落盘原文与渠道密钥 → 不自动续跑**（见 §9） |
| D12 | demo 的 `text` 超过 200 字符返回 **400**；Python 由 pydantic 返回 422 | 都是拒绝，但状态码不同 | 接受：422 的报错体是框架内部结构，与其它端点风格不一致 |
| D13 | demo 支持 `ADAPTER_DEMO_TRUST_XFF` | Go 新增，默认关 | 改进：反代后限额不再是全站共享 20 次 |
| D14 | `ELEVENLABS_MOCK=1` 时异步任务返回静音 WAV；Python 的 jobs 不判 MOCK，会**真打上游** | mock 环境跑任务会烧真实额度 | 改进。⚠️ M6 双跑时不要给 Python 侧开 mock 又跑 jobs 用例 |
| D15 | `Content-Type` 带 `charset=utf-8`（`application/json; charset=utf-8`）；Python 为 `application/json` | 逐头 diff 会报差异 | 接受，已进 M6 白名单 |
| D16 | 账本 JSONL 行的序列化格式：Python `{"ts": 1, ...}`（带空格），Go 紧凑 `{"ts":1,...}` | 字段名、字段顺序、字段值全同，只有行内空白不同 | 接受。**但按行做字节 diff 的对账工具必须先解析再比**，否则整本账全是假差异 |
| D17 | 送给上游的 `voice_settings.speed`：Go 输出 `1`，Python 输出 `1.0` | Go 的 `encoding/json` 把 float64(1.0) 写成 `1` | 接受：JSON 数字语义相同，上游按数值解析，不按字面量。已进 M6 白名单 |
| D18 | `X-Text-Clean` 值末尾的空格：Python `1 chars removed by `，Go `1 chars removed by` | HTTP 头值本就会被客户端 trim 首尾空白，客户看到的文本相同 | 接受。底层是两侧共有的缺陷（见 §10「共同缺陷」） |

---

## 8. 验收门禁（M6 影子对账的判据）

> `scripts/internal/` 下的脚本**不随本仓库发布**：它们要用一套内部参照实现做对照，
> 或绑定具体账号的音色与真实 Key。本节保留判据与结论 —— 换一份参照实现，方法照样成立。
> 仓库内可执行的验收是 `make check` / `make smoke` / `make acceptance` / `make crash-check`。

**比对项与判据**（实现见 `scripts/internal/m6_shadow.py`）：

| 比对项 | 判据 | 说明 |
|---|---|---|
| HTTP 状态码 | 100% 相等 | |
| 响应头（14 个） | 值 100% 相等 | 键名大小写不敏感（Go 侧绕过 canonical 化以保住驼峰）；`Content-Type` 只比主类型（D15） |
| `error.message` / `error.fix` / `error.code` | 100% 相等 | 比旧版更严：文案不一致 = 同一个错误两个客服口径 |
| 音频字节 | 逐字节相等 | 桩上游返回固定字节，排除上游抖动 |
| 发给上游的请求 | URL / JSON body / `xi-api-key` / `Accept` 全等 | body 按 key 排序后比，不比键顺序 |
| 账本行 | 17 个字段全等 | 只排除 `ts`（时钟）、`rid`（随机）、`ms`（耗时） |

**样本集**：33 条 × 3 种计费口径（`input` / `chars` / `upstream`）= 99 个组合。
覆盖：计费边界（首尾空格 / 零宽 / BOM / ZWJ / 代理对 / CRLF / 连续空白 / Markdown /
`text_clean` 三档）、4 种格式、语速钳制与字符串型 speed、4 类本地拒绝、
6 类上游错误（401/422/429/500/中文 message/无 `character-cost`）、3 类音色解析。

外加 4 条异步任务样本（`scripts/internal/m6_jobs.py`），比对提交响应、任务视图、下载字节与账本。

**白名单规则**：只有写进 `ACCEPTED`（带理由）的差异才算通过，且**报告中仍然逐条列出**。
未登记的差异一律算失败。白名单能吃掉的差异 = 这份门禁失效。

---

## 9. 异步任务表：落盘的边界在哪

**落盘的**（`ADAPTER_JOBS_DIR`）：

```
<id>.json    状态、字符数、时间、模型、voice_id、错误、usage_chars…
<id>.audio   成功任务的音频字节
```

原子写（`.tmp` + `fsync` + `rename`），权限 `0600`。目录不可写时**启动即失败**（开关打开时）——
否则「重启不丢」只是口号。

**刻意不落盘的**：

| 不落盘 | 原因 |
|---|---|
| `text`（客户原文） | 属于客户内容；重启后不需要它，就不该留在磁盘上 |
| `api_key`（渠道密钥） | 明文写盘等于把中转站渠道密钥交给任何能读文件的人 |

**因此不自动续跑，这是有意为之**：崩溃时状态为 `running` 的任务，上游很可能已合成完并扣费，
自动重跑 = 二次扣费 + 客户拿到两份互为重复的音频。恢复时一律转 `failed`：

| 中断前的状态 | 恢复后 | `never_started` | 给客户的 `fix` |
|---|---|---|---|
| `queued`（从未开始） | `failed` + `error_code=interrupted` | `true` | 直接重提，这一单没产生上游费用 |
| `running`（合成中） | 同上 | `false` | 重提前先确认：那一单可能已按已完成部分扣费 |

音频文件丢失（磁盘被清）→ `failed` + `error_code=audio_missing`，而不是给一份残缺音频。

**已知限制**：多副本部署时，轮询必须打到提交时的那个副本（任务表是本地的，不是共享存储）。
单副本或 sticky session 即可；真要横向扩展，把 `jobs.Store` 换成 Redis 实现，接口不用变。

---

## 10. M6 执行记录（2026-10-26）

### 影子期账本隔离（已拍板）

**Go 版与 Python 版不写同一个账本目录。** 两边各用自己的 `ADAPTER_BILLING_LOG_DIR`：

```
.m6/py/billing/2026-10-26.jsonl     # Python
.m6/go/billing/2026-10-26.jsonl     # Go
```

理由：写同一本账，同一笔请求会被记两次，`/v1/usage` 与 any 对账脚本的数字直接翻倍 ——
而且翻倍是"看起来很正常"的翻倍，最难发现。切完流量后再按需合并。

### 结果

| 口径 | 样本 | 未裁定差异 | 已裁定 |
|---|:--:|:--:|:--:|
| input | 33 | **0** | 2 |
| chars | 33 | **0** | 2 |
| upstream | 33 | **0** | 2 |
| jobs（异步） | 4 | **0** | 2 |

### 本轮修掉的真缺陷（不修就是上线事故）

| 缺陷 | 表现 | 影响 |
|---|---|---|
| `apierr.Encode` 走了 `json.Marshal` | `'eleven-<name>'` 变成 `eleven-\u003cname\u003e` | 客户照着报错去改配置，认不出那个占位符 |
| 提交响应读共享状态 | 同一个 202，快机器回 `running`、慢机器回 `queued` | 客户以为已开始合成，盯着不动的进度等；对账随机失败 |
| `voice_id` 报错用 `%q` | Python `'...'`，Go `"..."` | 客户在两周里看到两种排版 |
| fix 文案少一句 | 少了「不要传 tts-1 之外的自造名」 | 客户继续拿 `tts-1` 试 |
| 提示路径写成 `voices.json` | 少了 `app/` 前缀 | 客户在仓库根目录找不到文件 |

### 共同缺陷（两侧一致，但一致地错）

等价不等于正确。这三点比对不会报差异，必须单独记录：

1. **CRLF 归一化没有规则名** —— `1 chars removed by ` 后面是空的，客户不知道删了什么。
2. **`X-Usage-Rule` 的 `sent=` 永远不出现** —— 两侧都把这个位置的入参传成了 `in_chars`，
   而 input 口径下 `bill == in_chars`，于是 `raw != n` 恒为假。本意是告诉客户清理掉了多少，
   实际一句都没说过。
3. **`bill_source` 一个字段承载两种语义** —— 正常时是口径（input/chars/upstream），
   失败时变成结果（failed）。按它分组统计口径会多出一个假口径。

### M6-B 真上游抽样（2026-10-26 完成）

M6-A 用的是**确定性桩上游**，证明的是「两边行为逐字节等价」。
它证明不了「真上游也吃这套」—— 那需要渠道 Key 打真 ElevenLabs。本节就是那一轮。

真上游**无法逐字节比对**（音频编码不保证可复现、`character-cost` 按实际合成量算），
所以只验四项数值：`status` / `bill_chars` / `sent_chars` / `up_credits`，
外加一层上游自己的账反推我们的账。

**执行方式**：`python scripts/internal/m6b_real_upstream.py`（`--dry-run` 只打印计划不发请求）

| 项 | 取值 |
|---|---|
| 上游 | `https://api.elevenlabs.io/v1`（真实） |
| 音色 | `yun` → `4AfodMgwXps9oZFhHzoj` |
| 模型 | `eleven_multilingual_v2` |
| 口径 | `input`（生产默认，不跑另外两种：那是纯本地算术，M6-A 已证） |
| 样本 | 4 条 × 2 边 = 8 次真实合成，计费共 828 字符 |

**结果：门禁四元组 4/4 全等，账本合计全等，上游对账 392 = 196 + 196。**

| 样本 | 特征 | bill_chars | sent_chars | up_credits |
|---|---|:--:|:--:|:--:|
| b01 | 纯中文，无清理差异 | 15 | 15 | 7 |
| b02 | 连续空格 + 空行 | 23 | 19 | 9 |
| b03 | 零宽字符 + CRLF | 13 | 11 | 5 |
| b04 | 长文本 363 字 | 363 | 363 | 175 |

（两侧数值完全相同，故只列一份。）

#### 2026-10-30 复验：判据本身差点把一条正确的链路判死

复验（新一轮真实合成，828 字符）第一轮报告结论是**失败**：

```
门禁四元组 : 4/4 全等 ✅
账本合计   : 一致 ✅
上游对账   : 水位增量=217，期望 392 ❌
```

差 175，恰好是 b04 单边的数值。看起来像「上游少扣了一笔长文本」，实际不是。
顺着查下来是**三件事叠在一起**：

| # | 现象 | 真相 |
|---|---|---|
| 1 | 水位增量 217 | 对账脚本「只要水位比基线大就 break」，第一次轮询（3 秒）就已满足，最后一笔 15 秒的长文本根本没等 |
| 2 | 隔 3 分钟再查 `/v1/user` 仍是 269209 | 不是延迟，是那一笔还没被轮询进去 —— 脚本早就退出了，没有任何人在等 |
| 3 | `history` 里最后一条 `to=269167` 比前一条 `to=269209` 更小 | 上游 `character_count_change_to` 在密集请求下**不单调**，所以「事后水位 − 基线」这种算法会漏掉末笔 |

**结论：上游一分没少扣。** `/v1/history` 逐笔求和 = 175+175+5+5+9+9+7+7 = **392**，
与两边账本 196+196 严丝合缝。等到上游真正结算完，`/v1/user` 也返回 269384（增量 392）。

修完判据后重跑，同一脚本自己给出正确判定：

| 轮次 | 基线 | 事后 | 增量 | 判定 |
|---|---:|---:|---:|:--:|
| 判据未修 | 268992 | 269209 | 217 | ❌ 假失败 |
| 判据已修 | 269384 | 269776 | **392** | ✅ |

两处修改：

1. **轮询改成「连续 3 次读数不变才收工」**（上限 60 秒），只要还在涨就继续等。
   原来的「涨了就 break」在 8 连发场景下必然早退 —— 前几笔 3 秒内就结算了。
2. **水位差失真时用逐笔求和复核**：`/v1/history` 每条取 `to − from` 累加，
   不受 `to` 非单调影响。两者都平才算过，不平时两个数一起写进报告。

这条教训的价值高于这次抽样本身：**判据写错会把一条正确的链路报成事故，比不检查更危险。**
而且它只在「8 次请求密集连发 + 最后一笔特别长」时暴露 —— 单独跑一条永远碰不到。

#### 顺带测出来的两个硬数字

1. **上游真实倍率 `credits_per_char = 0.4804`** —— 来自 `character-cost` 回传，
   不是按「1 积分/字符」估的。定价与毛利必须用它，否则算出来的是「会计毛利率」。
2. **上游 `character_count` 增量 = 两边账本 `up_credits` 之和**（392 = 196 + 196）。
   这条是独立对账：上游的账由 ElevenLabs 结算，与我们的账本没有因果关系，
   所以它能反过来验我们的账。差值不为 0 只有三种解释 —— 我们记错了、别人在用这把 Key、
   上游按别的口径扣费，三种都必须当场看见。

#### ⚠️ 音色可用性：看得见 ≠ 能用

`lin`（`UFDAUkGzdLAEJlINT3Fx`）在 `GET /v1/voices` 里**明明列得出来**，
但 `sharing.status = "copied_disabled"`，真打上游直接：

```
403 authorization_error / voice_access_denied
"Voice 'UFDAUkGzdLAEJlINT3Fx' has been disabled by the owner."
```

`voices.json` 里的中文音色全部 `is_owner=false`，即从 Voice Library「抄」进账号的副本：

| 状态 | 音色 |
|---|---|
| `copied`（可用） | yun / pangge / bo / evan / amy / xinghe / anna / ladaisy / ziyue |
| `copied_disabled`（403） | **lin** |
| 列表里没有，但实测 200 | rachel / antoni / domi / elli / josh / arnold / sam（公共/legacy 音色，按 ID 直采可用） |

⚠️ 最后一行是**上一版的错结论，已纠正**：`GET /v1/voices` 不返回，不等于不可用。
这 7 个是 ElevenLabs 的公共/legacy 音色，不在账号列表里但按 voice_id 直接合成照样 200。
「列表里没有 ⇒ 不可用」这个推断是错的，唯一可靠的判据是真合成一次。
完整实测见 `VOICE_MATRIX_<key>.md`（`scripts/internal/_voice_matrix.py`，逐音色真打 1 字符）。

**这是生产级隐患**：库音色的作者一旦关闭共享，我方全账号立刻 403，代码一行没改也会挂。
所以 M6-B 前置了一道免费预检（`preflight_voice`），花钱之前先查 `sharing.status`。
**上线前必须核实生产渠道 Key 所在账号里每个在用音色的 `sharing.status`。**

#### `lin` 最终定性：作者关闭共享，不可恢复（2026-10-26 实测）

三条补救路径全部走死，别再试第四遍：

| 路径 | 调用 | 结果 |
|---|---|---|
| 重新添加副本 | `POST /v1/voices/add/{owner}/{voice_id}` | ❌ `400 Sharing is not enabled for voice '…'` |
| 用原始样本自建克隆 | `GET /v1/voices/{id}/samples/{sid}/audio` × 3 | ❌ `400 This sample does not exists on the server.` |
| 换音色 | — | ✅ 见 `output/go-adapter-migration/lin_replacement_20261026/` |

关键字段判读：`sharing.ban_reason = null`（**不是平台封禁**，封禁会填原因）、
`sharing.disable_at_unix = null`（**不是预告式停用**，到期关停会填这个）→ 作者侧主动关闭。
`sharing.date_unix = 2026-09-09 10:58 UTC` 是关闭时间点。

### ★ 音色可用性的正确判据（三重否定）

这一轮连续踩了两个「看起来对、其实无因果」的推断，一并钉死：

| 观测 | 错误推断 | 事实 |
|---|---|---|
| `GET /v1/voices` 列表里没有 | 「不可用」（404） | ❌ 那 7 个 legacy 音色按 ID 直采 200 |
| `GET /v1/shared-voices` 搜得到 | 「还在库里，能重新 add」 | ❌ 索引保留历史，共享开关已关，add 返 400 |
| `preview_url` HTTP 200 | 「音色本体还在，共享还开着」 | ❌ 只能证明作者自己那份还在，与共享无关 |

**唯一可靠判据：真调那次你想做的操作。** 想合成 → 真合成 1 字符；
想重新 add → 真调一次 add 看返不返 400。任何"读某个字段推断"的捷径都不可靠。

#### 已切换：`lin` → `xinghe`（2026-10-26 落地）

替代音色定为 `xinghe`（Xinghe Jiang - Magnetic Conversational，`lt7GBaCoAHWbT7JSZ5Xs`）。
选它的依据不是标签好看，而是三轮盲听筛出来的：`evan` / `bo` 被否（"一听就是外国人"），
`Sam` 的 `labels.accent` 直接是 `zh-taiwan mandarin`（台湾腔，排除），
`xinghe` 的标签与 `lin` 完全一致（male / young / language=zh / conversational）。

**语义变化，必须知道：**

- `voices.lin` 现在指向 `xinghe` 的 voice_id。**用 `lin` 这个名字调用，出声的已经是 xinghe。**
  保留这个名字只为不打断存量调用方（否则线上直接 400）。
- 旧 id `UFDAUkGzdLAEJlINT3Fx` 记录在 `voices.json` 的 `_deprecated_20261026` 段，
  仅供查历史订单的音色归属，**不要用于合成**。
- 这个映射横跨 `voices.json` 三处：`voices.lin`、`packs.cn.models["zh-duihua"]`、
  `packs.cn.models["zh-对话"]`。漏改任何一处，那条通路单独 403/404，
  而报错文案看起来像上游故障 —— 所以有 `TestLinMappedToXinghe` 把三处钉在一起，
  且该用例做过反证（注入单点漏改 → 必红）。

**验收记录**（`scripts/internal/verify_xinghe_rollout.py`，真上游，4 条通路 × 12 字符）：

| 通路 | 传参 | HTTP | 响应头 `X-ElevenLabs-Voice` | 模型 |
|---|---|---|---|---|
| `lin` | `voice=lin` | 200 | `lt7GBaCoAHWbT7JSZ5Xs` | `eleven_multilingual_v2` |
| `xinghe` | `voice=xinghe` | 200 | `lt7GBaCoAHWbT7JSZ5Xs` | `eleven_multilingual_v2` |
| `zh-duihua` | `model=zh-duihua` | 200 | `lt7GBaCoAHWbT7JSZ5Xs` | `eleven_v4` |
| `zh-对话` | `model=zh-对话` | 200 | `lt7GBaCoAHWbT7JSZ5Xs` | `eleven_v4` |

账本 4 行均为 `bill=12 sent=12 up=6 status=200`。

> **`zh-duihua` / `zh-对话` 必须放在 `model` 字段**，不能放 `voice`。
> 它们是 pack 里的**模型别名**（模型名即音色），走 `ResolveVoiceAndModel` 的
> `Aliases[model]` 分支；当成 `voice` 传会被 `VoiceToID` 判为未知短名（<15 rune）直接 400。
> 另外 `ADAPTER_PRESETS=none` 时包只是"不列出"，别名仍可调用 —— 上表第 3、4 行就是在
> 包全关的状态下验的，顺带证明了软门禁语义。

补充：`sharing.enabled_in_library` 也**不是**判据 —— 可用的 `yun` 该字段同样是 `false`。
判据要挑**有因果**的，不是挑"看起来相关的"。

#### Key 权限决定"能做哪些对账"

实测两把 Key 的权限范围（GET，免费）：

| 端点 | `sk_b0a…`（账号级） | `sk_dbb…`（渠道级，只开 TTS） |
|---|:--:|:--:|
| `/v1/user`、`/v1/user/subscription` | 200 | **401** 缺 `user_read` |
| `/v1/models` | 200 | **401** 缺 `models_read` |
| `/v1/voices` | 200 | 200 |
| `/v1/history` | 200 | 200 |
| `/v1/usage/character-stats` | 200 | 200（但返回空 `usage`，不可用） |
| 合成 `/v1/text-to-speech/{id}` | 200 | 200 |

所以 `/v1/user` 那套独立对账对渠道 Key 走不通。退路是 `/v1/history` 最新一条的
`character_count_change_to` 当水位 —— 权限更松，而且是**逐笔**口径，比总数更好用。
`m6b_real_upstream.py::snapshot_count` 会自动降级，报告里标注 `水位来源`。

两把 Key 实测结论一致：`sk_dbb…` 那一轮同样是**门禁四元组全等、水位增量 392 = 196 + 196**。

#### ⚠️ `character-cost` 头 ≠ 实际上游扣减

上游响应头按 `round(字符数 × 0.4804)` 给，但**每个请求至少扣 1 积分**：

| 字符数 | 响应头 | `/v1/history` 实际扣减 | 差 |
|:--:|:--:|:--:|:--:|
| 1 | 0 | 1 | **1** |
| 2 | 1 | 1 | 0 |
| 5 | 2 | 2 | 0 |
| 20 | 10 | 10 | 0 |
| 100 | 48 | 48 | 0 |

即 `实际 = max(1, 头)`。影响：`ADAPTER_BILLING_MODE=upstream` 若只信响应头，
**极短请求会按 0 计费** —— 客户白用、我们照付。绝对金额可以忽略（1 积分），
但口径要写对：`upstream` 口径取 `max(1, up_credits)` 才与上游一致。
完整数据见 `COST_BASIS_<key>.md`（`scripts/internal/_cost_basis_probe.py`）。

#### 两次失败尝试（记录在此，避免下次重复踩）

| 现象 | 真因 | 教训 |
|---|---|---|
| 全部 401 | 脚本里 `Authorization` 还留着 M6-A 的假 Key（`<case_id>_abcdefghij`，桩不校验） | 桩环境与真环境的鉴权差异必须在同一脚本内显式区分，不能靠「改一行」 |
| 全部 403 `voice_access_denied` | 选了 `lin`，该音色 `copied_disabled` | 音色状态必须预检，不能拿 `GET /v1/voices` 的「出现」当「可用」 |

两次失败都**没有产生费用**（401/403 不扣额度，`character_count` 前后一致），可放心重试。

#### 原始产物

`../output/go-adapter-migration/M6B_20261026/`：`M6B_report.md`（报告）、`M6B_raw.json`（逐样本原始响应，token 已脱敏）。



---
