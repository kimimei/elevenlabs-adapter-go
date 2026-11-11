# ElevenLabs → OpenAI 兼容适配器（Go 版）

把 ElevenLabs 的 TTS 包成 OpenAI 兼容接口（`POST /v1/audio/speech`）的单二进制适配器。
定位是**自托管网关之后的一个可选实现**：不绑定任何面板，也不要求替换现成的服务，
可以独立跑在 8123/8124 任意端口上，与既有实现并行。

> **当前状态：可用。** 契约见 [`docs/CONTRACT.md`](docs/CONTRACT.md)（对外行为的唯一验收依据），
> 已裁定差异、真上游抽样数值、音色可用性判据见 CONTRACT §7 / §8 / §10。
> 提交前的门禁是 `make check`（vet + 单测 + 格式）加 `make smoke` / `make acceptance` / `make crash-check`。


---

## 为什么要有 Go 版

**不是性能。** 瓶颈是上游 TTS 的 33ms/字 —— 一个 3000 字请求要跑 100 秒，
FastAPI 的框架开销占比不到 0.1%。任何以「Go 更快」立项的说法都站不住。

真实原因：

| # | 收益 | 具体表现 |
|---|---|---|
| 1 | 单静态二进制 | 9.3 MB，`CGO_ENABLED=0`，CentOS7 / glibc 2.17 直接跑，不用为运行时自编译 |
| 2 | 无 pip 供应链漂移 | 只有 1 个直接依赖（gin），Go 依赖不做安装期代码执行 |
| 3 | 与 new-api 同栈 | 将来可走 B 路线（内嵌 channel），消掉一跳 HTTP 和一份独立运维 |
| 4 | 类型安全契约 | 账本字段、状态码映射从字符串拼装变成 struct，改口径会在编译期炸 |
| + | 顺手修掉的欠账 | **任务表落盘（重启不再丢队列）**、并发用 `atomic` + defer（不再是 `_CONCURRENCY = [0]` 闭包 hack）、超时用 `context` 显式分支 |

---

## 快速开始

```bash
# 本地（mock，不消耗上游额度）
ELEVENLABS_MOCK=1 ADAPTER_PORT=8123 go run ./cmd/adapter

# 真实合成
ELEVENLABS_API_KEY=sk_xxx go run ./cmd/adapter
curl -s http://127.0.0.1:8123/healthz
```

编译与交叉编译：

```bash
make build          # 本机
make build-linux    # linux/amd64 静态二进制 → bin/adapter-linux-amd64
make docker         # 多阶段构建，镜像约 20MB
```

---

## 目录结构

```
cmd/adapter/main.go        只做装配：config → catalog → ledger → 上游 → 路由 → 优雅关闭
internal/config            26 个环境变量，名字与默认值必须与 Python 版一致
internal/voices            voices.json（//go:embed）+ 两层目录 + 软门禁
internal/textclean         三档清理；RE2 重写的 Markdown 规则
internal/upstream          ElevenLabs 调用、voice 解析、超时不重试、preview 缓存
internal/billing           三口径账本、聚合、毛利（积分与字符必须两边折美元再比）
internal/apierr            OpenAI 错误结构 + 上游原文 → 可执行的 fix
internal/pcm               裸 PCM → RIFF/WAVE
internal/jobs              任务表（落盘 + 原子写）、worker 池、取消、TTL 清理、重启恢复
internal/httpapi           路由、三道闸门、同步/异步端点、试听、目录、用量
internal/web               静态页（//go:embed）
```

`internal/upstream` 不依赖任何 HTTP 框架，只吃 `context` + 结构化参数 ——
将来走 B 路线时整段搬进 new-api，只换一层调用壳。

---

## 三条不能破的规矩

### 1. 数「字符」只能用 `utf8.RuneCountInString`

Go 的 `len(s)` 是**字节数**。中文一个字 3 字节，用错会让 `bill_chars` 直接差 3 倍，
且只在中文流量上发生 —— 测试用英文样本永远发现不了。

仓库里的 `textclean.RuneLen` 是唯一入口。`internal/{billing,httpapi}` 出现
`len(input)` / `len(text)` 一律视为 bug。

### 2. 中文不能进 HTTP 头

HTTP 头只允许 latin-1，中文写进去会让**整个响应**崩掉（不是只丢那个头，是请求直接 500）。
账本可以中文（`RuleText`），响应头必须 ASCII（`RuleHeader`）—— 两个函数，不能互相顶替。

### 3. 超时永不重试

上游是「合成完才返回」。客户端超时只代表自己不等了，上游那一侧通常已经扣费。
重试 = 再扣一次。实测 87% 的字符消耗来自超时重试。

`ELEVENLABS_MAX_RETRIES` 只对 5xx / 429 / 连接中断生效。

---

## 流式输出（SSE）

非流式要等整段合成完成才返回；流式让首块音频在合成过程中就到达播放器。
实测（真上游，18 字中文）：非流式 1825 ms，流式首块 **801 ms**。

```bash
curl -N -X POST http://127.0.0.1:8124/v1/audio/speech \
  -H "Authorization: Bearer sk_渠道密钥" -H 'Content-Type: application/json' \
  -d '{"model":"zh-boyin","input":"你好，这是流式合成。","response_format":"mp3","stream_format":"sse"}'
```

```
event: speech.audio.delta
data: {"type":"speech.audio.delta","audio":"SUQzBAA..."}

event: speech.audio.done
data: {"type":"speech.audio.done","usage":{"input_tokens":12,"output_tokens":0,"total_tokens":12,...}}
```

| 事件 | 含义 |
|---|---|
| `speech.audio.delta` | 一段 MP3 字节的 base64，按序拼接即完整音频 |
| `speech.audio.done` | 正常结束，**收到它才算完整**。载荷带 `usage`（与 Python 版同构） |
| `error` | 上游中途断了。状态码早已发出 200，只能靠这个事件知道失败 |

**四个必须知道的约束**

1. **只有 `stream_format: "sse"` 能开流式。** `stream: true` 不生效 —— 返回整段音频并带
   `X-Stream-Ignored: true`。这不是漏做：大量 Agent 框架照抄 OpenAI 默认参数会带 `stream: true`，
   直接 501 会让它们第一次调用就失败，而那些客户端拿到完整音频本来也能播。
2. **只支持 `response_format=mp3`。** WAV 要写完 RIFF 头才知道总长度，伪造头会让播放器读到 0 时长，
   比直接拒绝更难排查。传别的格式直接 400。
3. **经 new-api 转发时 `event:` 行会被网关剥掉**，客户端只剩 `data: {"type":...}`。
   适配器两处都写，所以直连可按事件名解析、经网关可按 JSON 里的 `type` 分派。
4. **流式不省钱。** 上游按完整文本计费，客户端中途断开也照扣，我们只是把账本记成 `499` 而非 `502`。
   要省钱请缩短文本，不是掐连接。

**记账**：流式与非流式写同一本账，用 `stream` / `stream_format` 标记，可据此算流式占比与首块 SLA。
成功时记上游真实积分 `up_credits`（拿不到记 `null`，绝不拿输入字符数冒充）；结局分 `200` 正常结束、
`499` 客户端断开、`502` 上游中断。

**一个容易静默漏掉的点**：ElevenLabs 的流式端点是 `/text-to-speech/{id}/stream`。少写 `/stream`
它照样返回 200，只是整段返回 —— 看起来「成功」，实际一点没流。仓库里有条测试专门钉这个后缀
（`TestStreamSSEHitsStreamEndpoint`），删掉后缀会立刻变红。

---

## 长文本必须走异步

同步接口在客户第一单就会失败：几千字的合成远超网关 60~120 秒的超时。
所以客户看到的是「提交成功 → 等 30 秒 → 504」，然后流失。

```bash
# 提交（立刻返回 202 + job_id）
curl -s -X POST http://127.0.0.1:8123/v1/audio/speech/jobs \
  -H "Authorization: Bearer sk_渠道密钥" -H 'Content-Type: application/json' \
  -d '{"model":"zh-boyin","input":"很长的文本…","response_format":"mp3"}'

# 轮询 → 下载
curl -s http://127.0.0.1:8123/v1/audio/speech/jobs/<id>
curl -s -o out.mp3 http://127.0.0.1:8123/v1/audio/speech/jobs/<id>/audio
```

**任务表落盘（`ADAPTER_JOBS_DIR`），这是 Go 版相对 Python 的实质差别：**

| 场景 | Python 版 | Go 版 |
|---|---|---|
| 进程重启后轮询已提交的任务 | **404**，任务凭空消失 | 返回真实终态 |
| 重启后下载已完成的结果 | 下载不到 | 从磁盘读回，字节一致 |
| 排队中被 kill 的任务 | 消失 | `failed` + `error_code=interrupted` + `never_started=true`（可安全重提） |
| 合成中被 kill 的任务 | 消失 | `failed` + `interrupted` + `never_started=false`（**可能已扣费，重提前先确认**） |

**刻意不自动续跑。** 崩溃时 `running` 的任务，上游很可能已合成完并扣费 —— 自动重跑就是二次扣费，
客户还会拿到两份互为重复的音频。同理，客户原文与渠道密钥**不落盘**（一个属于客户内容，
一个明文写盘等于泄露渠道密钥）。详见 CONTRACT §9。

---

## 在线试听（demo）

默认关闭。打开后用的是**服务端 Key**，等于把额度公开，所以有四道闸门：
单次 ≤ `ADAPTER_DEMO_MAX_CHARS`（默认 20）、每 IP 每小时 `ADAPTER_DEMO_PER_HOUR`（默认 20）次、
只允许白名单音色、`ADAPTER_DEMO_ENABLED=1` 才开。

⚠️ 挂在 nginx 后面时必须设 `ADAPTER_DEMO_TRUST_XFF=1`：
否则适配器看到的来源全是 `127.0.0.1`，20 次/小时变成**全站共享**，第 21 个访客起就点不动，
而告警上什么都看不出来。

---

## 测试

```bash
make check                             # 一切提交前的门禁：vet + 单测 + gofmt 干净度
go test ./...                          # 全部单元测试（64 个用例）
bash scripts/smoke.sh                  # 端到端冒烟：同步 + 异步 + 试听 + 一次真实重启（不花钱）
python scripts/internal/acceptance.py           # 功能可用性验收：36 项黑盒断言（不花钱）
python scripts/internal/crash_recovery_check.py # 崩溃恢复回归：真起进程、真 TerminateProcess
```

三条脚本的分工不同，别互相替代：

| 脚本 | 证明什么 | 证明不了什么 |
|---|---|---|
| `go test ./...` | 函数与分支的正确性 | 整条链路是否接通（路由/头/落盘） |
| `scripts/smoke.sh` | 关键链路真的通，含「杀进程再起来任务还在」 | 功能覆盖度（只管主线） |
| `scripts/internal/acceptance.py` | 每个对外功能都能用（目录、四种格式、清理档位、错误契约、异步、demo、usage） | 与历史实现是否逐字节等价 |
| `scripts/internal/crash_recovery_check.py` | 状态**真的**写到了磁盘上，且重启后不自动续跑 | 并发正确性（那条靠 `test-race`） |

> 这套门禁里踩过两个坑，写在这里免得后人重走：
> ①「断言写错」比「实现出错」更常见 —— 本仓库的验收脚本有 4 条断言最初写错了期望值
> （`/v1/voices` 返回的是 `voices` 不是 `data`、`queue_depth` 嵌在 `jobs` 下、
> 适配器本就不自带 API Key 鉴权、demo 用 `text` 字段），红灯全是假的；
> ② 状态码对不代表原因对 —— 缺 Key 拿到 401 有两种原因（`authentication_error` 与
> 上游拒绝假 Key 的 `upstream_error`），只看状态码会把假通过当成功。


## M6 影子对账：等价性的唯一可信证明

单元测试只证明局部（比如「记账函数算得对」），证明不了**整条链路** ——
路由、清理、上游转译、响应头、账本落盘，任何一环差一点，账就对不上。

影子对账的做法是：真起两个进程（被对照的实现 + 本适配器），同一批 33 条样本、
同一套确定性桩上游、两套**独立账本目录**，逐项比对：

> 对账脚本依赖一套内部的参照实现，未随本仓库发布（见 `.gitignore` 的 `scripts/internal/`）。
> 这里保留的是**判据与结论** —— 方法可复现，脚本本身换一份实现就得重写。


| 维度 | 比什么 |
|---|---|
| 下游响应 | 状态码、14 个响应头、音频字节（逐字节） |
| 上游请求 | URL、JSON body（键排序后）、`xi-api-key`、`Accept` |
| 账本 | 17 个字段（只排除 `ts` / `rid` / `ms`） |

**为什么上游要用桩而不是真 ElevenLabs**：等价性证明需要一个不动的参照系。
真上游每合成一次就扣一次额度，且音频字节不保证可复现 ——
一旦不一致，分不清是「适配器差异」还是「上游抖动」。真上游抽样是另一件事（M6-B）。

**影子期两边不写同一本账**：写同一本，同一笔请求会被记两次，
`/v1/usage` 的数字直接翻倍，而且翻倍看起来很正常，最难发现。
脚本里给两边各指一个 `ADAPTER_BILLING_LOG_DIR`。

当前结果：**99 个组合，0 未裁定差异**。
两处已裁定差异（`speed: 1` vs `1.0`、`X-Text-Clean` 尾部空格）连同理由写在
CONTRACT §7 D17/D18，报告里仍然逐条列出 —— 白名单能吃掉的差异，等于门禁失效。

## M6-B 真上游抽样：桩证明不了的最后一环

桩能证明「两边一致」，证明不了「真上游也吃这套」。真上游抽样用真实 Key 打
`api.elevenlabs.io`，4 条样本 × 2 边，只验四项数值
（`status` / `bill_chars` / `sent_chars` / `up_credits`）+ 一层上游反推。

结果：**4/4 全等，上游 `character_count` 增量 392 = 196 + 196，账面严丝合缝。**

三个来自真上游、桩永远给不了的数：

| 发现 | 数值 / 事实 |
|---|---|
| 上游真实倍率 | `credits_per_char = 0.4804`（不是 1 积分/字符，定价必须用它） |
| 独立对账成立 | 上游增量 = 两边 `up_credits` 之和，可分毫对应 |
| 音色可用性 | `lin` 在账号里列得出但 `copied_disabled`，真合成 403 |

**音色预检**：`preflight_voice` 在花钱之前免费查 `sharing.status`。
`is_owner=false` 的库音色作者一关闭共享，我方全账号立刻 403 —— 代码没改也会挂。
上线前必须逐个核实生产账号里在用音色的状态。

全量实测（每个音色真打 1 字符，实扣 1 积分）的结论：**19/20 可用**，唯一死掉的是 `lin`。
注意「`GET /v1/voices` 里没有」≠「不可用」—— rachel/antoni/domi/elli/josh/arnold/sam
不在列表里，按 ID 直采照样 200。

### `lin` 已确认不可恢复，已切换到 `xinghe`（2026-10-26）

作者关闭了共享。三条路径全部实测走死，别再重复尝试：

| 路径 | 结果 |
|---|---|
| `POST /v1/voices/add/{owner}/{voice_id}` 重新添加 | ❌ `400 Sharing is not enabled for voice` |
| 下载原始样本自建克隆（3 个 sample，约 39 分钟） | ❌ `400 This sample does not exists on the server.` |
| 换音色 | ✅ 已定为 `xinghe` |

**`voices.lin` 现在指向 `xinghe`（`lt7GBaCoAHWbT7JSZ5Xs`）。**
用 `lin` 这个名字调用，出声的已经是 xinghe —— 名字保留只为不打断存量调用方。
旧 id 记在 `voices.json` 的 `_deprecated_20261026` 段，仅供查历史归属，不要用于合成。

同一映射横跨三处（`voices.lin` / `packs.cn` 的 `zh-duihua` 与 `zh-对话`），
`TestLinMappedToXinghe` 把三处钉在一起，漏改任何一处都会让测试变红。

**判据只有一条：真调你想做的那次操作。**
`shared-voices` 搜得到、`preview_url` 能播、`enabled_in_library` 的取值 —— 都不是判据，
这些字段与实际准入没有因果关系。详见 CONTRACT「音色可用性的正确判据」。

切换验收的方法（脚本绑定内部账号，未随仓库发布）：

```bash
# 4 条通路各真合成一次，约 48 字符
# voices.lin / voices.xinghe / packs.cn 的 zh-duihua 与 zh-对话
```

> `zh-duihua` / `zh-对话` 是 **model 别名**，必须放 `model` 字段；放 `voice` 会被判未知短名 400。


验响应头与账号实际扣减的一致性时要注意：上游按
`max(1, round(字符 × 0.4804))` 扣，所以 1 字符请求的响应头报 0 而实际扣 1。
`ADAPTER_BILLING_MODE=upstream` 要照这个口径取 `max(1, up_credits)`。

**Key 权限决定能做哪些对账**：`/v1/user` 需要 `user_read`，渠道 Key 往往没开；
没有该权限时降级到 `/v1/history`（只要 `history_read`）取水位，并在报告里标注 `水位来源`。

`test-race` 需要 CGO + C 工具链（Linux/macOS 默认有，Windows 要装 mingw）。
本机没有 C 编译器，所以并发写账本这条底线由
`TestLedgerConcurrentWritesKeepLinesIntact` 兜住 —— 200 个 goroutine 同时落盘，
断言「行数精确 + 每行都是完整 JSON」—— 跑不出竞态，但能跑出丢行与串行。

（*注：该测试不能替代 `-race`，但它测的是真正会毁数据的那个后果：行被截断后，
对账工具会静默跳过坏行，一整天的账就悄悄少了几笔。）

`scripts/internal/crash_recovery_check.py` 是**唯一能证明「状态真的写到磁盘上」的检查** ——
`go test` 测的是恢复逻辑，测不出进程有没有把 running 落盘。它抓到过一个实测缺陷：
任务进入 running 时没落盘，强杀重启后按「从未开始」恢复，于是告诉客户「没产生费用，放心重提」，
而上游其实已经被调用过、可能已扣费。这条检查现在同时断言**措辞**（必须提示「可能已扣费」）。

`scripts/smoke.sh` 覆盖 **M4/M5 的关键行为**，其中包含一次真实的进程重启：
提交任务 → 轮询成功 → 下载 → `kill` 进程 → 重启 → 再查再下，断言状态与音频字节一致。
这条检查就是「重启丢队列」这个缺陷的回归测试。

关键门禁在 `internal/httpapi/speech_test.go::TestLedgerEqualsNewapiRuneCount`：

13 条样本逐笔断言 `账单 bill_chars == utf8.RuneCountInString(原始 input)`，零容差。
样本覆盖首尾空白、零宽、BOM、CRLF、emoji ZWJ、Markdown、代理对 ——
这些形态在真实中文流量里出现频率低，但一旦算错就是稳定且方向固定的差额，
肉眼不可见，只会在三个月后对账时炸出来。

---

## 与 Python 版的有意差异

见 [`docs/CONTRACT.md`](docs/CONTRACT.md) §7。摘要：

- Markdown 规则 9 条拆成 11 条（RE2 不支持反向引用 `\1`），语义等价
- Go 的 `\s` 不含 U+3000（Python 含），只影响 `normalize` 档
- 校验错误文案措辞不同，但 `code`/`type`/HTTP 状态码一致
- 静态页与 `voices.json` 打进二进制；`ADAPTER_STATIC_DIR` 可覆盖
- 任务表的 `chars` 用原始长度（不 strip），Python 用 strip 过的长度 —— 同一任务两边显示不同字符数
- 任务表落盘 + `error_code`/`never_started` 字段；demo 超长文本 400（Python 为 pydantic 422）

完整 15 条见 CONTRACT §7。

---

## 部署注意

**这个适配器不自带鉴权。** 它设计为位于中转站之后，由面板负责用户认证与扣费。
不要直接暴露公网 —— 前置网关，或用 `ADAPTER_ALLOW_IPS` 限制来源。

`ADAPTER_ALLOW_IPS` 填的是 **TCP 对端地址**，不是真实客户端 IP：
反代转发时这里看到的是反代自己的地址（同机 `127.0.0.1`、容器网关 `172.17.0.1`）。
这正是白名单该做的事 —— 只允许自己的反代来找适配器。

账本目录与任务表目录都必须可写。容器以 nonroot 运行，宿主目录要提前改属主：
`sudo chown -R 65532:65532 ./logs`
（`logs/billing` 是账本，`logs/jobs` 是任务表；任务表目录不可写时会**启动失败**，这是刻意的。）

---

## License

Apache-2.0，与 Python 版一致。
