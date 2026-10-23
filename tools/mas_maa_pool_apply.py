#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""给 MAS 队列分配第二个 MAA 核心（按队列项顺序单双分）。

示例：
  python tools/mas_maa_pool_apply.py `
    --map "并行3-MAA_3=E:\\MAA-Multi\\MAA_8" `
    --map "并行4-MAA_4=E:\\MAA-Multi\\MAA_9" --apply

规则：
  * 队列项第 1 个脚本保留原 MAA；
  * 第 2、4、6... 个脚本改到 second MAA；
  * 只改脚本 Info.Path，不改用户/模拟器/队列项；
  * 默认暂停队列，不自动恢复，方便验证。
"""
from __future__ import annotations

import argparse
import datetime as dt
import io
import json
import os
import shutil
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mas_inventory import post_json, safe_print  # noqa: E402

BASE = r'E:\AUTO-MAS'
CONFIG = os.path.join(BASE, 'config')


def queue_items(api: str, qid: str) -> list:
    data = post_json(api, '/api/queue/item/get', {'queueId': qid}).get('data') or {}
    out = []
    for iid, it in data.items():
        if iid == 'instances' or not isinstance(it, dict):
            continue
        sid = (it.get('Info') or {}).get('ScriptId')
        if sid and sid != '-':
            out.append({'itemId': iid, 'scriptId': sid})
    return out


def script_data(api: str, sid: str) -> dict:
    data = post_json(api, '/api/scripts/get', {'scriptId': sid}).get('data') or {}
    return data.get(sid) or {}


def main():
    ap = argparse.ArgumentParser(description='MAS 队列 MAA 池分配（默认 dry-run）')
    ap.add_argument('--map', action='append', default=[], required=True,
                    help='queueName=secondMaaPath，可重复')
    ap.add_argument('--mas-api', default=os.environ.get('MAS_API', 'http://127.0.0.1:36163'))
    ap.add_argument('--out', default='dist/maa_pool_apply.json')
    ap.add_argument('--apply', action='store_true')
    args = ap.parse_args()

    qs = post_json(args.mas_api, '/api/queue/get', {}).get('data') or {}
    by_name = {}
    for qid, q in qs.items():
        name = (q.get('Info') or {}).get('Name', '')
        by_name[name] = qid
    plans = []
    for spec in args.map:
        qname, sep, maa = spec.partition('=')
        if not sep:
            raise SystemExit('--map 格式：queueName=secondMaaPath')
        qid = by_name.get(qname)
        if not qid:
            raise SystemExit('找不到队列：%s' % qname)
        items = queue_items(args.mas_api, qid)
        if len(items) < 2:
            safe_print('SKIP %s：只有 %d 个队列项' % (qname, len(items)))
            continue
        changes = []
        for i, it in enumerate(items, 1):
            if i % 2 == 0:
                sd = script_data(args.mas_api, it['scriptId'])
                info = sd.get('Info') or {}
                changes.append({
                    'queue': qname,
                    'queueId': qid,
                    'itemIndex': i,
                    'itemId': it['itemId'],
                    'scriptId': it['scriptId'],
                    'scriptName': info.get('Name', ''),
                    'oldMaa': info.get('Path', ''),
                    'newMaa': maa,
                })
        q = qs[qid]
        info = q.get('Info') or {}
        plans.append({
            'queue': qname,
            'queueId': qid,
            'previousTimeEnabled': bool(info.get('TimeEnabled')),
            'secondMaa': maa,
            'changes': changes,
        })

    safe_print('MAA POOL PLAN（只读预览）')
    for p in plans:
        safe_print('  %s secondMaa=%s previousTimeEnabled=%s' % (
            p['queue'], p['secondMaa'], p['previousTimeEnabled']))
        for c in p['changes']:
            safe_print('     item#%d %-24s %s -> %s' % (
                c['itemIndex'], c['scriptName'], c['oldMaa'], c['newMaa']))
    if not args.apply:
        safe_print('dry-run：未写 MAS。加 --apply 才执行。')
        return 0

    ts = dt.datetime.now().strftime('%Y%m%d_%H%M%S')
    backup = os.path.join(BASE, 'backups', 'maa_pool_' + ts)
    os.makedirs(os.path.join(backup, 'config'), exist_ok=True)
    for fn in ('ScriptConfig.json', 'QueueConfig.json'):
        src = os.path.join(CONFIG, fn)
        if os.path.exists(src):
            shutil.copy2(src, os.path.join(backup, 'config', fn))
    with io.open(os.path.join(backup, 'scripts_api.json'), 'w', encoding='utf-8') as f:
        json.dump(post_json(args.mas_api, '/api/scripts/get', {}).get('data') or {}, f, ensure_ascii=False, indent=2)
    with io.open(os.path.join(backup, 'queues_api.json'), 'w', encoding='utf-8') as f:
        json.dump(qs, f, ensure_ascii=False, indent=2)

    # 暂停队列
    for p in plans:
        qid = p['queueId']
        q = post_json(args.mas_api, '/api/queue/get', {'queueId': qid}).get('data', {}).get(qid, {})
        info = q.get('Info') or {}
        info['TimeEnabled'] = False
        q['Info'] = info
        resp = post_json(args.mas_api, '/api/queue/update', {'queueId': qid, 'data': q})
        if resp.get('code') not in (200, None):
            raise SystemExit('暂停队列失败：%s %s' % (p['queue'], resp))
        safe_print('paused %s' % p['queue'])

    for p in plans:
        for c in p['changes']:
            sd = script_data(args.mas_api, c['scriptId'])
            info = sd.get('Info') or {}
            info['Path'] = c['newMaa']
            sd['Info'] = info
            resp = post_json(args.mas_api, '/api/scripts/update',
                             {'scriptId': c['scriptId'], 'data': sd})
            if resp.get('code') not in (200, None) or resp.get('status') == 'error':
                safe_print('UPDATE FAILED %s：%s' % (c['scriptName'], json.dumps(resp, ensure_ascii=False)))
                safe_print('队列保持暂停；请检查脚本锁后重试')
                return 2
            c['applied'] = True
            safe_print('updated %-24s -> %s' % (c['scriptName'], c['newMaa']))

    mapping = {
        'stage': 'applied',
        'createdAt': dt.datetime.now().isoformat(timespec='seconds'),
        'backupDir': backup,
        'plans': plans,
    }
    os.makedirs(os.path.dirname(args.out) or '.', exist_ok=True)
    with io.open(args.out, 'w', encoding='utf-8') as f:
        json.dump(mapping, f, ensure_ascii=False, indent=2)
        f.write('\n')
    safe_print('mapping: %s' % args.out)
    safe_print('backup: %s' % backup)
    safe_print('队列保持暂停；验证后再启用')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
