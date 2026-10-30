#!/usr/bin/env python
# -*- coding: utf-8 -*-
"""崩溃恢复回归：强杀进程后，任务表还在、且不会二次扣费。

为什么单独一条脚本
------------------
`go test` 能测「恢复逻辑」，但测不出「进程有没有把状态真的写到磁盘上」——
后者只有真启动一个进程、真把它 TerminateProcess 掉才看得见。

这条检查抓到过一个实测缺陷：任务进入 running 时没有落盘，磁盘上仍是 queued，
强杀重启后按「从未开始」恢复（never_started=true），于是告诉客户
「这一单没产生上游费用，放心重提」—— 而上游其实已经被调用过、可能已扣费。
一句错误的安慰比一句警告贵得多，所以这个断言的措辞也算在检查范围内。

用法（需要先 `go build -o bin/adapter ./cmd/adapter`）：
    python scripts/crash_recovery_check.py
"""
import io
import json
import os
import shutil
import subprocess
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding='utf-8', errors='replace')

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
BIN = os.path.join(ROOT, 'bin', 'adapter')
WORK = os.path.join(ROOT, '.crash-check')
JOBS = os.path.join(WORK, 'jobs')
BILL = os.path.join(WORK, 'billing')
PORT = 18131
STUB_PORT = 18500

if not os.path.exists(BIN):
    raise SystemExit('先编译：go build -o bin/adapter ./cmd/adapter')

shutil.rmtree(WORK, ignore_errors=True)

hits = {'n': 0}
release = threading.Event()


class Stub(BaseHTTPRequestHandler):
    """假上游：把请求挂住，模拟「正在合成」。"""

    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        self.rfile.read(n)
        hits['n'] += 1
        release.wait(30)

    def log_message(self, *a):
        pass


srv = ThreadingHTTPServer(('127.0.0.1', STUB_PORT), Stub)
threading.Thread(target=srv.serve_forever, daemon=True).start()

env = dict(os.environ,
           ELEVENLABS_MOCK='0',
           ELEVENLABS_API_BASE='http://127.0.0.1:%d/v1' % STUB_PORT,
           ELEVENLABS_TIMEOUT='60',
           ADAPTER_HOST='127.0.0.1', ADAPTER_PORT=str(PORT),
           ADAPTER_JOBS_ENABLED='1', ADAPTER_JOBS_DIR=JOBS,
           ADAPTER_JOBS_WORKERS='1', ADAPTER_BILLING_LOG_DIR=BILL)


def start():
    p = subprocess.Popen([BIN], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(80):
        try:
            urllib.request.urlopen('http://127.0.0.1:%d/healthz' % PORT, timeout=0.5)
            return p
        except Exception:
            time.sleep(0.1)
    raise SystemExit('适配器起不来')


def get(path):
    return json.loads(urllib.request.urlopen('http://127.0.0.1:%d%s' % (PORT, path), timeout=3).read())


fail = 0


def check(name, actual, expected):
    global fail
    if actual == expected:
        print('  ok   %-28s %s' % (name, actual))
    else:
        print('  FAIL %-28s 实际=%s 期望=%s' % (name, actual, expected))
        fail = 1


p = start()
body = json.dumps({'model': 'zh-boyin', 'input': '崩溃测试：这一单正在合成',
                   'response_format': 'mp3'}).encode()
req = urllib.request.Request('http://127.0.0.1:%d/v1/audio/speech/jobs' % PORT, data=body,
                             headers={'Content-Type': 'application/json',
                                      'Authorization': 'Bearer sk_crash_test'})
jid = json.loads(urllib.request.urlopen(req, timeout=3).read())['id']
print('job %s' % jid)

for _ in range(120):
    if get('/v1/audio/speech/jobs/' + jid)['status'] == 'running':
        break
    time.sleep(0.05)
check('进入 running', get('/v1/audio/speech/jobs/' + jid)['status'], 'running')

# 状态翻成 running 之后才会发上游请求，中间有几毫秒窗口 —— 等它到 1 再断言，
# 否则这里会随机失败（曾真实发生过：断言 0，随后重启检查里又显示累计 1）
for _ in range(80):
    if hits['n'] >= 1:
        break
    time.sleep(0.05)
check('上游被调用', hits['n'], 1)

# running 必须已经落盘，否则重启后会被误判成「从未开始」
time.sleep(0.2)
meta = os.path.join(JOBS, jid + '.json')
check('磁盘元数据存在', os.path.exists(meta), True)
check('磁盘状态 = running', json.load(open(meta, encoding='utf-8'))['status'], 'running')

p.kill()          # TerminateProcess：等价于 kill -9，不走优雅关闭
p.wait()
time.sleep(0.3)

p2 = start()
view = get('/v1/audio/speech/jobs/' + jid)
check('重启后状态', view['status'], 'failed')
check('重启后 error_code', view['error_code'], 'interrupted')
check('never_started=false', view['never_started'], False)
assert '可能已' in (view['fix'] or ''), '给客户的 fix 必须提示「可能已扣费」，不能安慰说没花钱：%r' % view['fix']

time.sleep(0.5)
check('重启后上游累计调用', hits['n'], 1)   # >1 = 自动续跑 = 二次扣费

p2.kill()
p2.wait()
release.set()
srv.shutdown()
shutil.rmtree(WORK, ignore_errors=True)

print()
print('全部通过。' if fail == 0 else '有失败项，见上。')
sys.exit(fail)
