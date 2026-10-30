#!/usr/bin/env bash
# 冒烟测试：不碰真实上游（MOCK=1），验证链路通、头齐、账本写得进去、任务表跨重启还在。
#
# 为什么单独给一个脚本而不是只留 make 目标：这台开发机没有 make，
# 而「本地能不能一键自证」比「Makefile 写得漂亮」重要。
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

PORT="${SMOKE_PORT:-18123}"
BASE="http://127.0.0.1:$PORT"
BILL_DIR=".smoke/billing"
JOBS_DIR=".smoke/jobs"
BIN="bin/adapter"

echo "== 编译 =="
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$BIN" ./cmd/adapter || exit 1

rm -rf .smoke
mkdir -p .smoke

export ELEVENLABS_MOCK=1
export ADAPTER_HOST=127.0.0.1
export ADAPTER_PORT="$PORT"
export ADAPTER_BILLING_LOG_DIR="$BILL_DIR"
export ADAPTER_JOBS_ENABLED=1
export ADAPTER_JOBS_DIR="$JOBS_DIR"
export ADAPTER_JOBS_WORKERS=1
export ADAPTER_JOBS_TTL_SECONDS=600
export ADAPTER_DEMO_ENABLED=1
export ADAPTER_DEMO_MAX_CHARS=5
export ADAPTER_DEMO_PER_HOUR=2

PID=""
start_adapter() {
  "$BIN" >>.smoke.log 2>&1 &
  PID=$!
  for _ in $(seq 1 50); do
    curl -sf "$BASE/healthz" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  echo "适配器起不来，看 .smoke.log"; return 1
}
stop_adapter() {
  [ -n "$PID" ] || return 0
  kill "$PID" 2>/dev/null
  wait "$PID" 2>/dev/null
  PID=""
}
trap 'stop_adapter; rm -f .smoke*.wav' EXIT

echo "== 启动（mock，端口 $PORT）=="
start_adapter || exit 1

fail=0
check() { # 名称 实际 期望
  if [ "$2" = "$3" ]; then
    printf '  ok   %-30s %s\n' "$1" "$2"
  else
    printf '  FAIL %-30s 实际=%s 期望=%s\n' "$1" "$2" "$3"
    fail=1
  fi
}
has() {   # 名称 内容 子串
  if printf '%s' "$2" | grep -q "$3"; then
    printf '  ok   %-30s 含 %s\n' "$1" "$3"
  else
    printf '  FAIL %-30s 不含 %s：%s\n' "$1" "$3" "$2"
    fail=1
  fi
}
hdr() { printf '%s' "$1" | tr -d '\r' | awk -v k="$2:" 'tolower($1)==tolower(k){print $2}'; }

echo "== 健康检查 =="
health=$(curl -sf "$BASE/healthz")
has healthz "$health" '"status":"ok"'
has 健康检查队列 "$health" '"queue_depth":0'
has 健康检查demo "$health" '"demo":true'

echo "== 模型列表（默认只含 5 个标准引擎）=="
n_models=$(curl -sf "$BASE/v1/models" | grep -o '"id"' | wc -l | tr -d ' ')
check "模型数量" "$n_models" "5"

echo "== 同步合成：中文 + 零宽 + 首尾空格 + wav =="
body='{"model":"eleven_multilingual_v2","voice":"rachel","input":"  你\u200b好世界  ","response_format":"wav"}'
hdrs=$(printf '%s' "$body" | curl -s -D - -o .smoke.wav -X POST "$BASE/v1/audio/speech" \
  -H 'Content-Type: application/json' --data-binary @-)

# 原始输入 9 个字符（含首尾空格与零宽）；实际合成 4 个
check "X-Usage-Input-Characters" "$(hdr "$hdrs" X-Usage-Input-Characters)" "9"
check "X-Text-Sent-Chars"        "$(hdr "$hdrs" X-Text-Sent-Chars)"        "4"
check "X-Usage-Source"           "$(hdr "$hdrs" X-Usage-Source)"           "input"
check "X-ElevenLabs-Voice"       "$(hdr "$hdrs" X-ElevenLabs-Voice)"       "21m00Tcm4TlvDq8ikWAM"
check "wav 容器"                 "$(head -c 4 .smoke.wav)"                 "RIFF"

echo "== 缺 voice 应 400 =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/audio/speech" \
  -H 'Content-Type: application/json' -d '{"model":"eleven_v3","input":"你好"}')
check "状态码" "$code" "400"

echo "== 异步任务：提交 -> 轮询 -> 下载 =="
submit=$(curl -s -X POST "$BASE/v1/audio/speech/jobs" -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk_smoke_key_1234' \
  -d '{"model":"zh-boyin","input":"  异步任务的计费基数也要是原始长度  ","response_format":"wav"}')
jid=$(printf '%s' "$submit" | sed -n 's/.*"id":"\([0-9a-f]\{16\}\)".*/\1/p')
[ -n "$jid" ] && printf '  ok   %-30s %s\n' "job_id" "$jid" || { printf '  FAIL 提交任务: %s\n' "$submit"; fail=1; }
has 提交返回队列位置 "$submit" '"queue_position":1'

status=""
for _ in $(seq 1 60); do
  poll=$(curl -s "$BASE/v1/audio/speech/jobs/$jid")
  status=$(printf '%s' "$poll" | sed -n 's/.*"status":"\([a-z]*\)".*/\1/p')
  [ "$status" = "succeeded" ] && break
  [ "$status" = "failed" ] && break
  sleep 0.1
done
check "任务最终状态" "$status" "succeeded"
has 结果含下载地址 "$poll" "/v1/audio/speech/jobs/$jid/audio"
has 结果含计费字符数 "$poll" '"usage_chars":20'

jh=$(curl -s -D - -o .smoke.job.wav "$BASE/v1/audio/speech/jobs/$jid/audio")
check "异步音频容器" "$(head -c 4 .smoke.job.wav)" "RIFF"
check "下载头 X-Usage-Input-Characters" "$(hdr "$jh" X-Usage-Input-Characters)" "20"

echo "== 异步任务账本 =="
all=$(cat "$BILL_DIR"/*.jsonl 2>/dev/null)
job_row=$(printf '%s\n' "$all" | grep "\"rid\":\"job:$jid\"" | tail -1)
has 异步行rid "$job_row" "\"rid\":\"job:$jid\""
has 异步行标记 "$job_row" '"async":true'
has 异步行in "$job_row" '"in_chars":20'
has 异步行sent "$job_row" '"sent_chars":16'
has 异步行bill "$job_row" '"bill_chars":20'

echo "== 重启后任务表还在（Python 版这里会 404）=="
stop_adapter
start_adapter || exit 1
after=$(curl -s "$BASE/v1/audio/speech/jobs/$jid")
has 重启后仍查得到 "$after" '"status":"succeeded"'
after_audio=$(curl -s -o .smoke.job2.wav -w '%{http_code}' "$BASE/v1/audio/speech/jobs/$jid/audio")
check "重启后仍能下载" "$after_audio" "200"
check "重启后音频容器" "$(head -c 4 .smoke.job2.wav)" "RIFF"
check "重启后音频字节一致" "$(wc -c < .smoke.job2.wav | tr -d ' ')" "$(wc -c < .smoke.job.wav | tr -d ' ')"

echo "== 在线试听（mock，单次上限 5 字符 / 每小时 2 次）=="
cfg=$(curl -sf "$BASE/v1/demo/config")
has demo授权 "$cfg" '"enabled":true'
has demo标签 "$cfg" '播音女声'
dh=$(printf '%s' '{"text":"你好世界你好世界","voice":"zh-boyin"}' | curl -s -D - -o .smoke.demo.wav \
  -X POST "$BASE/v1/demo/speech" -H 'Content-Type: application/json' --data-binary @-)
check "试听截断到 5 字符" "$(hdr "$dh" X-Demo-Chars)" "5"
check "试听音频容器" "$(head -c 4 .smoke.demo.wav)" "RIFF"
c2=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/demo/speech" \
  -H 'Content-Type: application/json' -d '{"text":"你好"}')
check "第 2 次试听" "$c2" "200"
c3=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/demo/speech" \
  -H 'Content-Type: application/json' -d '{"text":"你好"}')
check "第 3 次试听被限流" "$c3" "429"

echo "== 未开启时的错误形态 =="
# 关掉 job 开关后，提交应 404 而不是 500，且给出可执行的 fix
stop_adapter
export ADAPTER_JOBS_ENABLED=0
start_adapter || exit 1
off=$(curl -s -X POST "$BASE/v1/audio/speech/jobs" -H 'Content-Type: application/json' -d '{"input":"你好"}')
has 未开启返回错误 "$off" '异步任务接口未开启'
has 未开启给出修复建议 "$off" 'ADAPTER_JOBS_ENABLED=1'

echo
if [ "$fail" -eq 0 ]; then
  echo "全部通过。"
else
  echo "有失败项，见上。"
fi
exit "$fail"
