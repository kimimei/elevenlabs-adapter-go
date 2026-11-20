#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
接入自检 · 客户自助版（D2 自检 + D3 对数报告）

用途：让「接了适配器的人」在自己环境里一条命令验证两件事：
  1. 接没接对（12 项红绿，D2）
  2. 会不会算错钱（字符数 → quota → 美元 四段对数，D3）

和仓库里其它脚本的分工
----------------------
  acceptance.py / smoke.sh / crash_recovery_check.py  开发者自检（mock、不打真上游、证明代码对）
  selfcheck.py（本脚本）                              客户接入自检（打客户自己的环境、证明配置对）

两者不重复：本脚本**不测适配器内部实现**，只从外部 HTTP 看「这个接入会不会亏钱」。

设计约束
--------
  * 只用 Python 标准库（urllib3 都不用），客户环境零安装即可跑。
  * 不重试、不并发：超时一次即失败（上游合成完才返回，重试=再扣一次钱）。
  * 中文只进 JSON body，绝不进 HTTP 头。
  * 字符数用 len(str)（Python3 按 Unicode 码点计），等价 Go 的 utf8.RuneCountInString。

用法
----
  python3 selfcheck.py --base-url http://127.0.0.1:8123 --key sk_xxx --model zh-boyin

  # 走 new-api 时指向它的地址，Key 用 new-api 发的渠道 Key
  python3 selfcheck.py --base-url https://your-newapi.example --key sk-xxx --model <模型名>

  # 只跑联网 7 项、不查数据库
  python3 selfcheck.py --base-url ... --key ... --skip-db

  # 生成对数报告（会真合成一次，消耗字符额度）
  python3 selfcheck.py --base-url ... --key ... --reconcile

退出码：0 = 全绿；1 = 至少一项红；2 = 参数/环境错误。

判据来源：docs/CONTRACT.md（对外行为唯一验收依据）+ 方案的 D2 12 项规格。
"""

import argparse
import json
import re
import ssl
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone

VERSION = "1.0.0"

# 上游真实倍率（credits_per_char，来自 character-cost 回传实测）——仅用于报告展示，
# 判据本身不依赖它（判据只看「发送字符数 == 响应头字符数」这个可自证的事实）。
CREDITS_PER_CHAR = 0.4804

GREEN = "[ OK ]"
RED = "[FAIL]"
WARN = "[WARN]"
INFO = "[ .. ]"

results = []


def record(no, name, ok, detail="", fatal=True):
    results.append({"no": no, "name": name, "ok": ok, "detail": detail, "fatal": fatal})
    tag = GREEN if ok else (WARN if not fatal else RED)
    print(f"  {tag} {no:>2}. {name}" + (f"  — {detail}" if detail else ""))


def http(url, method="GET", headers=None, body=None, timeout=30):
    """单次请求，不重试。返回 (status, headers_dict, body_bytes)。"""
    data = None
    h = dict(headers or {})
    if body is not None:
        data = body.encode("utf-8") if isinstance(body, str) else body
        h.setdefault("Content-Type", "application/json")
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    ctx = ssl.create_default_context()
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as r:
            return r.status, dict(r.headers.items()), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers.items()), e.read()
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        return 0, {}, str(e).encode("utf-8", "replace")


def mask(key):
    if not key:
        return "(空)"
    return key[:6] + "..." + key[-4:] if len(key) > 12 else "***"


def build_text(n=25):
    """固定测试文本：纯中文常用字，无生僻字，避免读音/编码噪声影响字符数判据。"""
    base = "你好这是一段用于接入自检的测试文本今天天气不错我们开始合成语音"
    s = (base * ((n // len(base)) + 1))[:n]
    return s


# ---------------------------------------------------------------------------
# D2 · 联网 7 项（适配器/网关外部可见）
# ---------------------------------------------------------------------------
def check_network(base, key, model):
    print("\n[A] 接入自检 · 联网 7 项（不查数据库）")
    hdr = {"Authorization": "Bearer " + key, "User-Agent": f"adapter-selfcheck/{VERSION}"}

    # 1. healthz
    st, _, _ = http(base.rstrip("/") + "/healthz", headers=hdr, timeout=10)
    record(1, "服务存活 /healthz", st == 200, f"HTTP {st}")
    if st != 200:
        # 后续项强依赖服务在，但继续跑以暴露更多问题（只标非致命，避免噪音）
        print(f"  {WARN} 服务未就绪，后续项结果可能全部失败，请先修第 1 项")

    # 2. Key 有效（用 /v1/models 试探，401/403 即 Key 问题）
    st, h, body = http(base.rstrip("/") + "/v1/models", headers=hdr, timeout=10)
    key_ok = st not in (401, 403)
    record(2, "Key 有效", key_ok, f"HTTP {st}（401/403=Key 错或无权限）")

    # 3. /v1/models 非空
    models = []
    if st == 200:
        try:
            j = json.loads(body)
            models = j.get("data", []) if isinstance(j, dict) else []
        except Exception:
            pass
    record(3, "/v1/models 非空", bool(models), f"返回 {len(models)} 条")

    # 4. 请求的模型在目录内
    ids = [m.get("id") for m in models if isinstance(m, dict)]
    in_catalog = (model in ids) if ids else None
    if in_catalog is None:
        record(4, "模型在目录内", False, "目录为空，无法判断", fatal=False)
    else:
        record(4, "模型在目录内", in_catalog, f"{model} {'命中' if in_catalog else '未命中'}；目录样例={ids[:3]}")

    # 5+6+7. 真合成一次（这三项共用同一次请求，避免重复扣费）
    text = build_text(25)
    payload = json.dumps(
        {"model": model, "input": text, "response_format": "mp3"}, ensure_ascii=False
    )
    st, h, audio = http(
        base.rstrip("/") + "/v1/audio/speech",
        method="POST",
        headers=hdr,
        body=payload,
        timeout=60,
    )
    lower = {k.lower(): v for k, v in h.items()}

    # 5. 音色/模型可解析 = 合成请求成功（200）；403 多为音色无权限
    synth_ok = st == 200
    detail5 = f"HTTP {st}"
    if st == 403:
        detail5 += "（403 常见：音色共享被关/无权限，不是 Key 问题）"
    record(5, "音色可解析 / 合成成功", synth_ok, detail5)

    # 6. 返回音频可解码（至少不是 JSON 错误体）
    is_audio = st == 200 and len(audio) > 128 and not audio.lstrip()[:1] in (b"{", b"[")
    if st == 200 and audio[:3] == b"ID3" or audio[:2] in (b"\xff\xfb", b"\xff\xf3", b"\xff\xf2"):
        is_audio = True
    record(6, "返回音频有效（非错误体）", is_audio, f"{len(audio)} 字节，前 4 字节={audio[:4]!r}")

    # 7. 响应头有字符数，且数字等于发送字符数（计费链路断没断的直接证据）
    # 注意：只判「头存在」不够 —— 头在但数字被改，照样多扣钱却会误报全绿。
    usage_char = lower.get("x-usage-input-characters")
    has_usage = usage_char is not None
    usage_match = has_usage and str(usage_char).strip().isdigit() and int(usage_char) == len(text)
    if not has_usage:
        d7 = f"头缺失；发送字符数={len(text)}（计费链路断）"
    elif not usage_match:
        d7 = f"★头回 {usage_char} ≠ 发送 {len(text)} 字符（会被多扣/少扣）"
    else:
        d7 = f"值={usage_char} == 发送 {len(text)} 字符"
    record(7, "响应头字符数 == 发送字符数", usage_match, d7)

    return {
        "base": base,
        "key": mask(key),
        "model": model,
        "sent_chars": len(text),
        "head_chars": int(usage_char) if usage_char and str(usage_char).isdigit() else None,
        "status": st,
    }


# ---------------------------------------------------------------------------
# D2 · new-api 侧 5 项（输出 SQL，客户照抄到自己的库跑；不自动连库）
# ---------------------------------------------------------------------------
def check_db_manual(model, sent_chars):
    print("\n[B] 接入自检 · new-api 侧 5 项（生成 SQL，你在自己的数据库里跑）")
    print("    自动连库风险高（库名/表结构/账号权限各环境不同），所以只给可复制的判据 SQL。")
    print(f"    把 <MODEL> 替换为实际模型名（本次={model}），在 new-api 的 MySQL 里执行：\n")

    checks = [
        (8, "模型已注册 channels.models",
         f"SELECT id,name FROM channels WHERE models LIKE '%{model}%';",
         "有行=已注册；空行=404 前兆"),
        (9, "abilities 行存在",
         f"SELECT model,channel_id FROM abilities WHERE model='{model}' LIMIT 5;",
         "有行=可调；空行=模型广场看不到"),
        (10, "ModelRatio 已配",
         f"SELECT `key`,`value` FROM options WHERE `key`='ModelRatio';",
         "JSON 里应有该模型键；缺键=按默认价静默计费"),
        (11, "AudioRatio = 0（★最贵的一项）",
         f"SELECT `key`,`value` FROM options WHERE `key`='AudioRatio';",
         "★必须为空或 0；非 0 会在字符费之上再乘倍率，实测多收 5.19 倍"),
        (12, "日志 quota 与字符数对得上",
         f"SELECT id,model,quota,other FROM logs WHERE model='{model}' ORDER BY id DESC LIMIT 3;",
         f"本次发送 {sent_chars} 字符；quota 应为 字符数×ModelRatio，偏差 0 才算对"),
    ]
    for no, name, sql, judge in checks:
        print(f"  {INFO} {no:>2}. {name}")
        print(f"        SQL: {sql}")
        print(f"        判读: {judge}")
        results.append({"no": no, "name": name, "ok": None, "detail": "待客户库内确认", "fatal": False})


# ---------------------------------------------------------------------------
# D3 · 四段对数报告
# ---------------------------------------------------------------------------
def reconcile(info):
    print("\n[C] D3 · 四段对数报告")
    sent = info["sent_chars"]
    head = info["head_chars"]
    print("  ┌─────────────────────────────────────────────────────────────┐")
    print("  │ 段1 发送字符数        │ %-6s │ 脚本 len(input) 计            │" % sent)
    if head is None:
        print("  │ 段2 响应头字符数      │ %-6s │ ⚠ 无响应头，计费链路可疑     │" % "缺失")
    else:
        flag = "一致" if head == sent else "★不一致"
        print("  │ 段2 响应头字符数      │ %-6s │ %s                       │" % (head, flag))
    print("  │ 段3 new-api 日志quota │ ______ │ 跑上面第 12 项 SQL 取值     │")
    print("  │ 段4 折算美元          │ ______ │ quota ÷ (倍率×字符) 反推    │")
    print("  └─────────────────────────────────────────────────────────────┘")
    if head is None:
        print(f"  {RED} 段2 缺失：响应头没回字符数，计费链路是断的 —— 这是比「算错」更严重的情况，先修第 7 项。")
    elif head != sent:
        print(f"  {RED} 段1≠段2：发送 {sent} 字符、响应头回 {head}。这是计费链路断的直接证据，先修再谈钱。")
    else:
        print(f"  {GREEN} 段1=段2={sent}：字符数没有在链路里被吞或被改。")
    print(f"  上游倍率参考：{CREDITS_PER_CHAR} credits/char（仅参考，判据不依赖它）。")
    print("  完整四段一致（偏差 0）才算「接了不会亏钱」。段3/段4 填好后自己留存这份报告。")


def main():
    ap = argparse.ArgumentParser(description="适配器接入自检（D2）+ 对数报告（D3）· 客户自助版")
    ap.add_argument("--base-url", required=True, help="适配器或 new-api 的地址，例 http://127.0.0.1:8123")
    ap.add_argument("--key", required=True, help="访问 Key（不会打印出来）")
    ap.add_argument("--model", default="zh-boyin", help="测试用模型名（默认 zh-boyin）")
    ap.add_argument("--skip-db", action="store_true", help="不生成数据库 SQL 判据")
    ap.add_argument("--reconcile", action="store_true", help="生成 D3 对数报告（会真合成一次，消耗额度）")
    args = ap.parse_args()

    if not args.base_url.startswith(("http://", "https://")):
        print("错误：--base-url 必须以 http:// 或 https:// 开头", file=sys.stderr)
        return 2

    print("=" * 63)
    print(f" 适配器接入自检 v{VERSION}   {datetime.now(timezone.utc).astimezone().strftime('%Y-%m-%d %H:%M:%S %z')}")
    print("=" * 63)
    print(f" 目标: {args.base_url}")
    print(f" Key : {mask(args.key)}   Model: {args.model}")
    print(" 说明: 本脚本从外部 HTTP 验证接入配置，不测适配器内部实现。")

    info = check_network(args.base_url, args.key, args.model)
    if not args.skip_db:
        check_db_manual(args.model, info["sent_chars"])
    if args.reconcile:
        reconcile(info)

    bad = [r for r in results if r["ok"] is False and r["fatal"]]
    print("\n" + "=" * 63)
    if bad:
        print(f" 结论: {len(bad)} 项致命失败 → " + ", ".join(f"#{r['no']}{r['name']}" for r in bad))
        print(" 联网项修复后可重跑；DB 项(8-12)按上面 SQL 自行核对。")
        return 1
    unknowns = [r for r in results if r["ok"] is None]
    print(f" 结论: 联网项全绿；{len(unknowns)} 项 DB 判据待你在库内核对。")
    if unknowns:
        print(" ★ 重点确认第 11 项 AudioRatio=0 —— 它是唯一「配错看不出来、但持续多收钱」的项。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
