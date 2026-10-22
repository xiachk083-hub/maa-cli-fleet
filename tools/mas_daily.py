#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MAS 日常脚本执行（单账号 canary，只对已存在账号）。

默认 dry-run；--apply 才调用 POST /api/dispatch/start（mode=ScriptConfig，不走队列）。
安全闸：
  * fleet 里该账号 enabled=true -> BLOCKED（避免双控）；
  * MAS 账号 Status=false -> BLOCKED；
  * 当前游戏日已有 MAS history -> BLOCKED（除非 --force，仍不建议）；
  * 只接受已存在的 MAS user/script，不创建、不修改任何配置。

用法（机端）：
  python tools/mas_daily.py --account a01
  python tools/mas_daily.py --account a01 --apply --wait --timeout 1800
"""
from __future__ import annotations

import argparse
import datetime as dt
import io
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mas_inventory import (  # noqa: E402
    collect_fleet, collect_history, collect_mas, collect_queues, game_day,
    history_for, norm, post_json, safe_print, mask_id,
)

DEFAULT_API = os.environ.get('MAS_API', 'http://127.0.0.1:36163')
DEFAULT_HISTORY = os.environ.get('MAS_HISTORY', r'E:\AUTO-MAS\history')


def find_target(account: str, accounts, users):
    fleet_acc = next((a for a in accounts if str(a.get('id')) == account), None)
    cands = []
    if fleet_acc:
        name = str(fleet_acc.get('name') or '')
        emu = str(fleet_acc.get('emu') or '')
        cands = [u for u in users if norm(u.get('name')) == norm(name)]
        if not cands:
            cands = [u for u in users if str(u.get('emulatorIndex')) == emu]
    else:
        cands = [u for u in users if u.get('userId') == account or norm(u.get('name')) == norm(account)]
    if len(cands) != 1:
        return fleet_acc, None, '账号匹配到 %d 个 MAS 用户（需要唯一）' % len(cands)
    return fleet_acc, cands[0], ''


def newest_history_mtime(root: str, day: str, name: str) -> float:
    d = os.path.join(root, day, name)
    if not os.path.isdir(d):
        return 0.0
    mt = 0.0
    for fn in os.listdir(d):
        if fn.lower().endswith(('.log', '.json')):
            try:
                mt = max(mt, os.path.getmtime(os.path.join(d, fn)))
            except OSError:
                pass
    return mt


def latest_history_json(root: str, day: str, name: str, after_ts: float):
    d = os.path.join(root, day, name)
    if not os.path.isdir(d):
        return None
    files = [os.path.join(d, f) for f in os.listdir(d) if f.lower().endswith('.json')]
    fresh = []
    for p in files:
        try:
            if os.path.getmtime(p) >= after_ts - 2:
                fresh.append(p)
        except OSError:
            pass
    if not fresh:
        return None
    p = max(fresh, key=os.path.getmtime)
    try:
        with io.open(p, 'r', encoding='utf-8-sig') as f:
            data = json.load(f)
        data['_file'] = p
        return data
    except Exception:
        return None


def log_line(msg: str) -> None:
    safe_print('[%s] %s' % (dt.datetime.now().strftime('%H:%M:%S'), msg))


def main():
    ap = argparse.ArgumentParser(description='MAS 单账号日常 canary')
    ap.add_argument('--account', required=True, help='fleet 账号 ID 或 MAS 账号名')
    ap.add_argument('--mas-api', default=DEFAULT_API)
    ap.add_argument('--mas-history', default=DEFAULT_HISTORY)
    ap.add_argument('--fleet-root', default=os.path.abspath(os.path.join(os.path.dirname(__file__), '..')))
    ap.add_argument('--apply', action='store_true', help='真执行 dispatch/start（默认 dry-run）')
    ap.add_argument('--unsafe-direct', action='store_true', help='已弃用：直接 ScriptConfig，不经过 MAS 调度台/队列')
    ap.add_argument('--force', action='store_true', help='允许当天已有 history 再跑（不建议）')
    ap.add_argument('--wait', action='store_true', help='等待 history 结果')
    ap.add_argument('--timeout', type=int, default=1800)
    ap.add_argument('--ledger', default='', help='执行台账 JSON')
    args = ap.parse_args()

    scripts, users = collect_mas(args.mas_api)
    accounts, tasks, day = collect_fleet(args.fleet_root)
    hist = collect_history(args.mas_history)
    queues = collect_queues(args.mas_api)
    fleet_acc, user, err = find_target(args.account, accounts, users)
    if err or user is None:
        raise SystemExit('BLOCKED: ' + (err or '找不到 MAS 账号'))

    script = next((s for s in scripts if s['scriptId'] == user['scriptId']), None)
    if script is None:
        raise SystemExit('BLOCKED: 账号所在脚本不是 MAA 脚本')
    auto_queues = [q for q in queues if q.get('timeEnabled') and user['scriptId'] in q.get('scripts', [])]

    h = history_for(hist, user.get('name', ''))
    today_hist = day in h.get('days', [])
    mt = newest_history_mtime(args.mas_history, day, user.get('name', ''))
    recent_min = (time.time() - mt) / 60 if mt else 9999

    reasons = []
    if fleet_acc and fleet_acc.get('enabled'):
        reasons.append('fleet 已启用该账号（会双控）')
    if not user.get('status'):
        reasons.append('MAS 账号 Status=false')
    if auto_queues and args.unsafe_direct and not args.force:
        reasons.append('脚本在自动队列：' + ', '.join(q.get('name', '') for q in auto_queues))
    if today_hist and not args.force:
        reasons.append('当前游戏日已有 MAS history（防重复刷）')
    if recent_min < 15 and not args.force:
        reasons.append('15 分钟内有 history 活动（可能在跑）')

    safe_print('=== MAS daily canary ===')
    safe_print('gameDay=%s account=%s masName=%s status=%s' % (day, args.account, user.get('name'), user.get('status')))
    safe_print('script=%s user=%s emu=%s server=%s stage=%s' % (
        script.get('name'), mask_id(user.get('userId')), script.get('emulatorIndex'),
        user.get('server'), user.get('stage')))
    safe_print('history: today=%s latest=%s days=%d recent=%.1fmin' % (
        'yes' if today_hist else 'no', h.get('latest') or '-', len(h.get('days', [])), recent_min))
    safe_print('autoQueue: %s' % (', '.join(q.get('name', '') for q in auto_queues) or 'no'))
    if auto_queues and not args.unsafe_direct:
        safe_print('WARN: 脚本也在自动队列；MAS 脚本锁会串行化，不会双开')
    if reasons:
        for r in reasons:
            safe_print('BLOCKED: ' + r)
        if not args.apply:
            safe_print('(dry-run，未执行)')
        return 2

    if not args.apply:
        mode = 'ScriptConfig' if args.unsafe_direct else 'AutoProxy'
        target = user['userId'] if args.unsafe_direct else script['scriptId']
        safe_print('PLAN: POST /api/dispatch/start {"mode":"%s","taskId":"%s"}' % (mode, target))
        safe_print('dry-run：未执行。加 --apply 才真发。')
        return 0

    mode = 'ScriptConfig' if args.unsafe_direct else 'AutoProxy'
    target = user['userId'] if args.unsafe_direct else script['scriptId']
    start = time.time()
    resp = post_json(args.mas_api, '/api/dispatch/start', {'mode': mode, 'taskId': target})
    task_id = str(resp.get('taskId') or '')
    log_line('DISPATCH mode=%s target=%s code=%s status=%s taskId=%s' % (mode, target, resp.get('code'), resp.get('status'), task_id))
    if resp.get('code') not in (200, None) or not task_id:
        safe_print('执行失败：%s' % json.dumps(resp, ensure_ascii=False))
        return 1

    ledger_path = args.ledger or os.path.join(os.path.abspath(args.fleet_root), 'dist', 'mas_daily_ledger.json')
    os.makedirs(os.path.dirname(ledger_path) or '.', exist_ok=True)
    ledger = {'runs': []}
    if os.path.exists(ledger_path):
        try:
            with io.open(ledger_path, 'r', encoding='utf-8-sig') as f:
                ledger = json.load(f)
        except Exception:
            ledger = {'runs': []}
    run = {
        'account': args.account,
        'masName': user.get('name', ''),
        'scriptId': script['scriptId'],
        'userId': user['userId'],
        'mode': mode,
        'target': target,
        'taskId': task_id,
        'gameDay': day,
        'startedAt': dt.datetime.now().isoformat(timespec='seconds'),
        'status': 'dispatched',
        'historyFile': '',
        'maaResult': '',
        'sanity': None,
    }
    ledger.setdefault('runs', []).append(run)
    with io.open(ledger_path, 'w', encoding='utf-8') as f:
        json.dump(ledger, f, ensure_ascii=False, indent=2)
        f.write('\n')
    safe_print('ledger: %s' % ledger_path)

    if not args.wait:
        safe_print('已派发。用 history/日志看结果，或加 --wait。')
        return 0

    log_line('等待 history 结果（最多 %ds）…' % args.timeout)
    deadline = time.time() + args.timeout
    data = None
    while time.time() < deadline:
        data = latest_history_json(args.mas_history, day, user.get('name', ''), start)
        if data:
            break
        time.sleep(10)
    if not data:
        # 超时不再把 MAS 任务留在后台：先停掉，再记 timeout。
        try:
            post_json(args.mas_api, '/api/dispatch/stop', {'taskId': task_id})
            run['note'] = 'no history before timeout; dispatch stopped'
            log_line('TIMEOUT → dispatch/stop taskId=%s' % task_id)
        except Exception as e:
            run['note'] = 'no history before timeout; stop failed: %s' % e
            log_line('TIMEOUT → stop failed taskId=%s：%s' % (task_id, e))
        run['status'] = 'timeout'
        run['endedAt'] = dt.datetime.now().isoformat(timespec='seconds')
        with io.open(ledger_path, 'w', encoding='utf-8') as f:
            json.dump(ledger, f, ensure_ascii=False, indent=2)
            f.write('\n')
        safe_print('TIMEOUT：没有等到新的 history JSON（已尝试停止 MAS 任务）')
        return 3

    result = str(data.get('maa_result') or '')
    if '完成' in result or '成功' in result:
        status = 'finished'
    elif '失败' in result or '异常' in result:
        status = 'failed'
    else:
        status = 'unknown'
    run.update({'status': status, 'historyFile': data.get('_file', ''), 'maaResult': result,
                'sanity': data.get('sanity'), 'endedAt': dt.datetime.now().isoformat(timespec='seconds')})
    with io.open(ledger_path, 'w', encoding='utf-8') as f:
        json.dump(ledger, f, ensure_ascii=False, indent=2)
        f.write('\n')
    log_line('RESULT=%s maa_result=%s sanity=%s file=%s' % (status, result, data.get('sanity'), os.path.basename(data.get('_file', ''))))
    return 0 if status == 'finished' else 4


if __name__ == '__main__':
    main()
