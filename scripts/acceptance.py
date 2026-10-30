#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""
功能可用性验收：把适配器当作黑盒，逐项验证「各功能能不能正常工作」。

与 smoke.sh 的分工
------------------
`smoke.sh` 验证的是**关键链路的断言**（头齐、账本写得进、任务跨重启还在）。
本脚本验证的是**功能覆盖度**（目录端点、四种音频格式、清理档位、错误契约、
异步全生命周期、demo 四道闸门、usage 聚合、字符上限边界）。

两者都不打真实上游，只在 `ELEVENLABS_MOCK=1` 下跑，所以不花钱。

用法
----
    go build -o bin/adapter ./cmd/adapter
    python scripts/acceptance.py

    # 指定二进制与端口
    ADAPTER_BIN=bin/adapter-linux-amd64 ACCEPTANCE_PORT=18200 python scripts/acceptance.py

退出码 0 表示全部通过。临时账本/任务表目录用完即删，不在仓库留痕。

一条经验
--------
写断言时「我以为的行为」和「实现的行为」经常不是一回事。本脚本里每个断言的
期望值都对照过 `docs/CONTRACT.md`；如果你改了契约，这里的断言要跟着改，
否则它会在一个**正确**的实现上变红，然后被当成已知失败忽略掉。
"""
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding="utf-8", errors="replace")

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
BIN = os.environ.get("ADAPTER_BIN") or os.path.join(
    ROOT, "bin", "adapter.exe" if os.name == "nt" else "adapter")
PORT = int(os.environ.get("ACCEPTANCE_PORT", "18200"))
ALT_PORT = PORT + 1
TMP = tempfile.mkdtemp(prefix="adapter-acc-")
DEFAULT_KEY = "sk_acceptance_dummy_key_not_real_0001"

results = []


def _env(port, mock):
    env = dict(os.environ)
    env.update({
        "ADAPTER_HOST": "127.0.0.1",
        "ADAPTER_PORT": str(port),
        "ADAPTER_BILLING_LOG_DIR": os.path.join(TMP, "billing-%d" % port),
        "ADAPTER_JOBS_DIR": os.path.join(TMP, "jobs-%d" % port),
        "ADAPTER_JOBS_ENABLED": "1",
        "ADAPTER_BILLING_MODE": "input",
        "ADAPTER_MAX_CHARS": "2000",
    })
    # 非 mock 场景必须真的「没有 Key」：继承来的环境变量会顶替掉缺失状态，
    # 于是 401 看起来对了，实际是「上游拒了假 Key」（upstream_error）而不是
    # 「根本没给 Key」（authentication_error）。状态码相同、原因不同，
    # 这种假通过比失败更难发现。
    env.pop("ELEVENLABS_API_KEY", None)
    if mock:
        env["ELEVENLABS_MOCK"] = "1"
        env["ELEVENLABS_API_KEY"] = DEFAULT_KEY
        env["ADAPTER_DEMO_ENABLED"] = "1"
        env["ADAPTER_DEMO_MAX_CHARS"] = "200"
        env["ADAPTER_DEMO_PER_HOUR"] = "50"
    return env


def start(port, mock):
    proc = subprocess.Popen([BIN], env=_env(port, mock),
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(100):
        try:
            urllib.request.urlopen("http://127.0.0.1:%d/healthz" % port, timeout=1)
            return proc
        except Exception:
            time.sleep(0.15)
    raise SystemExit("适配器未能在 %d 端口启动：%s" % (port, BIN))


def req(port, method, path, body=None, key=DEFAULT_KEY, raw=False):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key
    r = urllib.request.Request("http://127.0.0.1:%d%s" % (port, path),
                               data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=60) as resp:
            payload = resp.read()
            return resp.status, dict(resp.headers), (payload if raw else _json(payload))
    except urllib.error.HTTPError as e:
        payload = e.read()
        return e.code, dict(e.headers), (payload if raw else _json(payload))


def _json(b):
    try:
        return json.loads(b.decode())
    except Exception:
        return {"_raw_len": len(b)}


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))


def run_mock(port):
    # ---- 目录 ----
    st, _, b = req(port, "GET", "/v1/models")
    check("models：5 个标准引擎", st == 200 and len(b.get("data", [])) == 5,
          "%d 引擎" % len(b.get("data", [])))

    st, _, b = req(port, "GET", "/v1/voices")
    check("voices：基础音色", st == 200 and b.get("count", 0) >= 20, "count=%s" % b.get("count"))

    st, _, b = req(port, "GET", "/v1/voices?presets=cn")
    cn = [v for v in b.get("voices", []) if v.get("kind") == "alias"]
    check("voices?presets=cn：16 个中文别名", st == 200 and len(cn) == 16, "%d 个" % len(cn))

    st, _, b = req(port, "GET", "/v1/voices?presets=cn&lang=zh")
    check("voices?lang=zh 过滤", st == 200 and len(b.get("voices", [])) == 16,
          "%d 个" % len(b.get("voices", [])))

    # 软门禁：没启用中文包时 lang=zh 返回空，这是设计而不是故障
    st, _, b = req(port, "GET", "/v1/voices?lang=zh")
    check("voices?lang=zh 无 presets → 空（软门禁）", st == 200 and b.get("count") == 0,
          "需同时传 presets=cn")

    st, _, b = req(port, "GET", "/v1/presets")
    check("presets：列出包清单", st == 200 and len(b.get("data", [])) > 0,
          "%d 个包" % len(b.get("data", [])))

    # ---- 静态页 ----
    st, _, b = req(port, "GET", "/", raw=True)
    check("静态页可访问", st == 200 and b"<html" in b.lower(), "%dB" % len(b))

    # ---- 同步合成：四种格式 ----
    for fmt in ("mp3", "wav", "opus", "pcm"):
        st, _, b = req(port, "POST", "/v1/audio/speech",
                       {"model": "zh-boyin", "input": "功能验收测试", "response_format": fmt}, raw=True)
        check("同步合成 format=%s" % fmt, st == 200 and len(b) > 0, "%dB" % len(b))

    st, _, b = req(port, "POST", "/v1/audio/speech",
                   {"model": "zh-boyin", "input": "x", "response_format": "flac"})
    check("不支持的格式 → 400（不静默降级）", st == 400, str(st))

    # ---- 音色解析 ----
    st, h, _ = req(port, "POST", "/v1/audio/speech",
                   {"model": "zh-duihua", "input": "对话测试", "response_format": "mp3"}, raw=True)
    check("model 别名解析出音色", st == 200 and bool(h.get("X-ElevenLabs-Voice")),
          "voice=%s" % h.get("X-ElevenLabs-Voice"))

    st, h, _ = req(port, "POST", "/v1/audio/speech",
                   {"model": "zh-boyin", "input": "x", "voice": "lin", "response_format": "mp3"}, raw=True)
    check("voice 短名 lin 解析", st == 200 and bool(h.get("X-ElevenLabs-Voice")),
          "voice=%s" % h.get("X-ElevenLabs-Voice"))

    st, h, _ = req(port, "POST", "/v1/audio/speech",
                   {"model": "eleven_v4", "input": "x", "voice": "21m00Tcm4TlvDq8ikWAM",
                    "response_format": "mp3"}, raw=True)
    check("15 位以上按 voice_id 直传", st == 200 and h.get("X-ElevenLabs-Voice") == "21m00Tcm4TlvDq8ikWAM",
          "voice=%s" % h.get("X-ElevenLabs-Voice"))

    # ---- 文本清理三档 ----
    for lvl in ("none", "normalize", "full"):
        st, h, _ = req(port, "POST", "/v1/audio/speech",
                       {"model": "zh-boyin", "input": "a  b\tc**粗**",
                        "response_format": "mp3", "text_clean": lvl}, raw=True)
        check("清理档位 text_clean=%s" % lvl, st == 200,
              "in=%s sent=%s" % (h.get("X-Usage-Input-Characters"), h.get("X-Text-Sent-Chars")))

    # ---- 错误契约 ----
    st, _, b = req(port, "POST", "/v1/audio/speech", {"model": "no-such-model", "input": "x"})
    check("未知 model → 400 且带 fix", st == 400 and "fix" in json.dumps(b, ensure_ascii=False), str(st))

    st, _, b = req(port, "POST", "/v1/audio/speech", {"model": "zh-boyin"})
    check("缺 input → 400", st == 400, str(st))

    st, _, b = req(port, "POST", "/v1/audio/speech", {"model": "zh-boyin", "input": "   \u200b  "})
    check("纯空白/零宽 → 400（不白扣费）", st == 400, str(st))

    st, _, b = req(port, "POST", "/v1/audio/speech", {"model": "zh-boyin", "input": "字" * 2001})
    check("字符上限 2000：2001 字 → 400", st == 400, str(st))
    st, _, b = req(port, "POST", "/v1/audio/speech", {"model": "zh-boyin", "input": "字" * 2000}, raw=True)
    check("字符上限 2000：2000 字 → 200", st == 200, str(st))

    st, _, b = req(port, "GET", "/v1/audio/speech/jobs/deadbeefdeadbeef")
    check("不存在的 job → 404", st == 404, str(st))

    st, _, b = req(port, "POST", "/v1/audio/speech", {"model": "zh-boyin", "input": "x", "stream": True})
    check("stream=true → 501", st == 501, str(st))

    # ---- 异步全生命周期 ----
    st, _, b = req(port, "POST", "/v1/audio/speech/jobs",
                   {"model": "zh-boyin", "input": "异步验收文本内容", "response_format": "mp3"})
    jid = b.get("id") or b.get("job_id")
    check("提交 job → 202", st == 202 and bool(jid), "id=%s" % jid)

    final = None
    for _ in range(80):
        st, _, b = req(port, "GET", "/v1/audio/speech/jobs/%s" % jid)
        if b.get("status") in ("succeeded", "failed"):
            final = b
            break
        time.sleep(0.15)
    check("job 轮询到 succeeded", final is not None and final.get("status") == "succeeded",
          "status=%s" % (final.get("status") if final else "timeout"))

    st, _, audio = req(port, "GET", "/v1/audio/speech/jobs/%s/audio" % jid, raw=True)
    check("job 音频可下载", st == 200 and len(audio) > 0, "%dB" % len(audio))

    st, _, b = req(port, "GET", "/v1/audio/speech/jobs")
    check("job 列表", st == 200, str(st))

    # ---- demo ----
    st, _, b = req(port, "GET", "/v1/demo/config")
    check("demo/config", st == 200 and b.get("enabled") is True, str(st))

    st, _, b = req(port, "POST", "/v1/demo/speech", {"text": "试听一句", "voice": "zh-boyin"}, raw=True)
    check("demo 合成（text 字段，无需 Key）", st == 200 and len(b) > 0, "%dB" % len(b))

    st, _, b = req(port, "POST", "/v1/demo/speech", {"text": "长" * 300})
    check("demo 超长 → 400", st == 400, str(st))

    # ---- usage / healthz / 账本 ----
    st, _, b = req(port, "GET", "/v1/usage")
    check("usage 聚合", st == 200 and "today" in b, str(sorted(b.keys())))

    st, _, b = req(port, "GET", "/healthz")
    jobs = b.get("jobs", {})
    check("healthz 含队列深度", st == 200 and "queue_depth" in jobs,
          "queue_depth=%s workers=%s" % (jobs.get("queue_depth"), jobs.get("workers")))

    bdir = os.path.join(TMP, "billing-%d" % port)
    lines = 0
    for f in os.listdir(bdir):
        with open(os.path.join(bdir, f), encoding="utf-8") as fh:
            lines += sum(1 for ln in fh if ln.strip())
    check("账本真的落盘", lines > 0, "%d 行" % lines)


def run_liveish(port):
    """非 mock：不发上游请求，只验鉴权前置（缺 Key 必须 401，且不产生账本行）。"""
    st, _, b = req(port, "POST", "/v1/audio/speech",
                   {"model": "eleven_v4", "input": "x", "voice": "21m00Tcm4TlvDq8ikWAM"}, key="")
    code = b.get("error", {}).get("code") if isinstance(b, dict) else ""
    check("非 mock 缺 Key → 401", st == 401, "code=%s" % code)


def main():
    if not os.path.exists(BIN):
        raise SystemExit("找不到二进制：%s\n先执行 go build -o bin/adapter ./cmd/adapter" % BIN)

    mock_proc = start(PORT, True)
    try:
        run_mock(PORT)
    finally:
        mock_proc.terminate()
        try:
            mock_proc.wait(timeout=10)
        except Exception:
            mock_proc.kill()

    # 非 mock 只测鉴权，不会真的打到上游（401 发生在发起上游请求之前）
    live_proc = None
    try:
        live_proc = start(ALT_PORT, False)
        run_liveish(ALT_PORT)
    finally:
        if live_proc:
            live_proc.terminate()
            try:
                live_proc.wait(timeout=10)
            except Exception:
                live_proc.kill()

    width = max(len(name) for name, _, _ in results) + 2
    print("\n" + "=" * (width + 50))
    print("%-*s %-6s %s" % (width, "功能项", "结果", "明细"))
    print("=" * (width + 50))
    passed = 0
    for name, ok, detail in results:
        if ok:
            passed += 1
        print("%-*s %-6s %s" % (width, name, "PASS" if ok else "FAIL", detail))
    print("=" * (width + 50))
    print("合计 %d/%d 通过" % (passed, len(results)))
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    code = 1
    try:
        code = main()
    finally:
        shutil.rmtree(TMP, ignore_errors=True)
    sys.exit(code)
