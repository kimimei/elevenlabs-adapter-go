BINARY  := adapter
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath

.PHONY: all build build-linux build-arm64 test test-race vet fmt tidy check smoke acceptance crash-check docker clean \
        release-check release-check-selftest \
        m6 m6-jobs m6b m6b-real m6b-matrix m6b-cost voice-lin-check voice-lin-alternatives voice-verify voice-verify-dry

all: build

## 本机编译
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/adapter

## 交叉编译：目标机是 CentOS7 / glibc 2.17，静态编译绕开一切运行时依赖
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64 ./cmd/adapter

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-arm64 ./cmd/adapter

## 测试。账本是并发写入点，改 bill/ledger 相关一定要跑 test-race
test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

## 提交前的一站式门禁：格式 + 静态检查 + 单测 + 两条黑盒回归
check: vet test
	gofmt -l . | (! grep .) || (echo "gofmt 有未格式化文件，先跑 make fmt" && false)

## 发布前门禁：扫「会被提交的内容」（已跟踪 + 未忽略的新文件）和 git 历史，
## 拦真实 IP / 内部域名 / 令牌 / 私钥 / 本机路径 / 填了真值的 .env.example。
## 规则与 Python 版 scripts/release_check.py 同源，实现在 internal/releasecheck。
## 私有域名清单放 .release-check.local（不进仓库），CI 里用 RELEASE_CHECK_INTERNAL_HOSTS 注入。
release-check:
	go run ./cmd/release-check

## 门禁自身的反证：造一份故意违规的仓库，确认每条规则真的会红
release-check-selftest:
	go run ./cmd/release-check -self-test

## 冒烟：不起真实上游，覆盖同步 + 异步 + 试听 + 一次真实重启
## 逻辑只在 scripts/smoke.sh 里写一份 —— 两处实现必定漂移
smoke:
	bash scripts/smoke.sh

## 功能可用性验收：黑盒逐项验证目录/格式/清理档位/错误契约/异步/demo/usage
acceptance: build
	python scripts/acceptance.py

## 崩溃恢复回归：真起进程、真 TerminateProcess，验证任务表落盘且不自动续跑
crash-check: build
	python scripts/crash_recovery_check.py

docker:
	docker build -t elevenlabs-adapter-go:$(VERSION) .

clean:
	rm -rf bin .smoke .crash-check

# ---------------------------------------------------------------------------
# 以下目标依赖 scripts/internal/，该目录不随公开仓库发布。
# 原因：这些脚本要么引用内部的 Python 实现做影子对账，要么绑定某个具体账号的
# 音色与一把真实 Key，对外既跑不起来、也包含不该外露的账号信息。
# 方法论（影子对账的判据、真上游抽样的四项数值）保留在 docs/CONTRACT.md §8/§10。
# ---------------------------------------------------------------------------

## M6 影子对账：真起两个进程、真发 HTTP、两套独立账本、逐字段比对（不花钱）
## 等价性证明的唯一可信来源。单元测试只证明局部，这条证明整条链路。
m6-all: build
	python scripts/internal/m6_shadow.py --mode all

## 异步任务对账
m6-jobs: build
	python scripts/internal/m6_jobs.py

## M6-B 真上游抽样：⚠️ 会真实消耗额度（约 830 字符 / 8 次合成）
m6b: build
	python scripts/internal/m6b_real_upstream.py --dry-run

m6b-real: build
	python scripts/internal/m6b_real_upstream.py

## 音色/模型可用性实测：每个音色真合成 1 字符（实扣 1 积分）
m6b-matrix:
	python scripts/internal/_voice_matrix.py

## 响应头 character-cost 与账号实际扣减的口径对比（约 130 字符）
m6b-cost:
	python scripts/internal/_cost_basis_probe.py

## lin 音色诊断：只读，不改任何东西
voice-lin-check:
	python scripts/internal/fix_lin_voice.py --dry-run

## 生成 lin 替代候选样音（约 228 字符）
voice-lin-alternatives:
	python scripts/internal/make_lin_replacements.py

## lin -> xinghe 切换验收：4 条通路真上游各合成一次（约 48 字符）
voice-verify:
	python scripts/internal/verify_xinghe_rollout.py

voice-verify-dry:
	python scripts/internal/verify_xinghe_rollout.py --dry-run

m6: m6-all m6-jobs
