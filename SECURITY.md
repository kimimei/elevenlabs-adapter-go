# 安全说明

> 版本：0.5.0（2026-11-11）｜ 适用范围：本仓库全部代码与默认部署方式

## 一、这个组件是什么、不是什么

**是**：跑在你内网 / 容器网络里的协议转换层，把 ElevenLabs 包装成 OpenAI 兼容接口。

**不是**：一个可以直接暴露到公网的 API 网关。它**没有内建用户体系、没有鉴权、没有配额**——
按设计，鉴权与计费由上游的中转站（new-api / one-api）负责。

这条边界决定了下面所有内容的前提：**适配器的安全边界 = 谁能连到它的端口**。

## 二、部署边界（必读）

| 要求 | 原因 | 做法 |
|---|---|---|
| **不要把 8123 直接暴露公网** | 任何能连上该端口的人都能消耗你配置的 Key | 只监听 `127.0.0.1`；或让 new-api 走容器网络访问 |
| 必须对外时，加来源白名单 | 同上 | `ADAPTER_ALLOW_IPS=172.18.0.1` 这种精确列表，含 `/v1/usage` |
| 容器不要用 root | 降低逃逸后的影响面 | 镜像已 `USER nonroot:nonroot` |
| Key 只放在渠道配置里 | 适配器不需要知道你的中转令牌 | 渠道密钥填 ElevenLabs Key；适配器侧 `.env` 可留空 |
| 显式设定并发上限 | 默认 `0` 表示不限，过载时表现为随机失败而非可控拒绝 | `ADAPTER_MAX_CONCURRENCY` |
| 限制业务侧文本长度 | 单次请求默认上限 2 MiB | `ADAPTER_MAX_CHARS` |

## 三、已实现的安全措施

| # | 措施 | 位置 | 防的是什么 |
|---|---|---|---|
| 1 | `voice_id` 白名单正则 `^[A-Za-z0-9_-]{10,64}$` | `internal/upstream/resolve.go` | 构造含 `/`、`..` 的 voice_id 把请求指到上游其它路径（路径穿越），且带的是服务端 Key |
| 2 | 白名单校验 **TCP 对端地址**，不读 `X-Forwarded-For` | `internal/httpapi/middleware.go` | 伪造 XFF 绕过白名单。用框架默认的 `c.ClientIP()` 就会信任该头，等于把白名单交给客户端自己填 |
| 3 | 请求体上限同时覆盖 `Content-Length` 与 chunked | `internal/httpapi/middleware.go` | 用 `Transfer-Encoding: chunked` 发超大请求绕过长度检查，打满内存 |
| 4 | 本地明文 Key 回落**默认关闭** | `ADAPTER_ALLOW_LOCAL_KEY=0` | 共享主机上，任何调用方白嫖运营者 `~/.elevenlabs/config.json` 里的个人 Key |
| 5 | 上游超时**默认不重试** | `ELEVENLABS_MAX_RETRIES=0` | TTS 按字符计费，重试成功后上游扣两次；流式一旦开始写字节更不可能重试 |
| 6 | 账本只记调用方 Key 的**前 6 位** | `internal/httpapi/app.go::tokenLabel` | 排障需要「是哪把 Key」，但不该把完整密钥落盘 |
| 7 | 并发闸门返回 `429` + `Retry-After` | `internal/httpapi/middleware.go` | 无上限时过载表现为成片 502，运维和客户都看不出是容量问题 |
| 8 | 试听限额默认按 TCP 对端计 | `ADAPTER_DEMO_TRUST_XFF=0` | 信任 XFF 会让 20 次/小时的限额形同虚设（该开关只在确认前面是自己的反代时才开） |
| 9 | 发布门禁扫敏感信息 | `make release-check` / `internal/releasecheck` | 真实 IP、内部域名、令牌、私钥、填了真值的 `.env.example` 混进公开仓库 |

第 2 条值得展开：它的代价是**反代后面看到的永远是反代的地址**，白名单得填反代的 IP 而不是真实客户端 IP。
这是刻意的取舍——真实客户端 IP 只能靠一个可伪造的头获得，那样白名单就成了摆设。
需要「按真实客户端 IP 限流」时，正确做法是在反代层（nginx `limit_req` / `allow` / `deny`）做。

## 四、已知限制（不是漏洞，但要清楚）

| 限制 | 说明 | 缓解 |
|---|---|---|
| 无内建鉴权 | 设计如此，鉴权在 new-api | 网络边界 + `ADAPTER_ALLOW_IPS` |
| `/v1/usage` 无鉴权，且含调用方 Key 前缀与用量 | 它能回答「谁用了多少、花了多少」，属于运营数据 | 与代理接口一同置于内网；对公网暴露时务必开 `ADAPTER_ALLOW_IPS` |
| 白名单填的是 TCP 对端 | 见上，反代转发时是反代地址 | 按部署形态实测确认该填谁 |
| 无按调用方限流 | 单一进程、单一 Key 池 | 在 new-api 侧做令牌限速；或部署多实例 |
| 异步任务表在进程内存 | 重启丢排队中的任务；多副本需 sticky session | 长文本任务失败后重新提交 |
| 账本文件含来源 IP 与 Key 前 6 位 | 排障所需，但属于可识别信息 | 已在 `.gitignore` 中排除 `/logs/`；对外提供前先脱敏 |
| 错误信息可能带上游响应片段 | 便于定位被上游拒绝的原因 | 生产环境收紧日志级别 |
| 容器内无 TLS | 预期由反代终结 TLS | 反代配置证书 |

## 五、报告漏洞

请**不要**开公开 issue —— 公开的漏洞细节等于给所有部署实例发通知。

唯一入口是 GitHub 的 **Security → Report a vulnerability**（[Security 页](https://github.com/kimimei/elevenlabs-adapter-go/security)）。
报告会进私密 advisory，可以在里面直接讨论、提交补丁、申请 CVE，全程不公开；也不需要你留下邮箱。

请附：复现步骤、影响范围、你的环境（版本 / 部署方式 / 是否在反代之后）。48 小时内回复。

如果漏洞已存在公开仓库的历史里（例如某次提交误带了密钥），**只告诉我，不要自己开 PR 清理**——
历史重写需要协调，贸然 force-push 会让其他人的克隆失效。
