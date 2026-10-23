#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MAS 队列清理 Apply（默认 dry-run）。

本工具只做一件事：
  * 把名字含“临时”的队列改名为“归档-原名称”，并强制 TimeEnabled=False；
  * 不删除队列、不改队列项、不动正式 并行* 队列。

用法：
  python tools/mas_queue_apply.py --stage archive
  python tools/mas_queue_apply.py --stage archive --apply
  python tools/mas_queue_apply.py --stage restore --mapping dist/mas_queue_cleanup.json --apply
"""
from __future__ import annotations

import argparse
import datetime as dt
import io
import json
import os
import shutil
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mas_inventory import post_json, safe_print  # noqa: E402

BASE = r'E:\AUTO-MAS'
CONFIG = os.path.join(BASE, 'config', 'QueueConfig.json')


def queue_data(api: str, qid: str) -> dict:
    data = post_json(api, '/api/queue/get', {'queueId': qid}).get('data') or {}
    return data.get(qid) or {}


def is_temp(name: str) -> bool:
    return '临时' in str(name or '') and not str(name or '').startswith('归档-')


def archive(api: str, out_path: str, apply: bool) -> int:
    qs = post_json(api, '/api/queue/get', {}).get('data') or {}
    rows = []
    for qid, q in qs.items():
        info = q.get('Info') or {}
        name = str(info.get('Name') or '')
        if not is_temp(name):
            continue
        new_name = '归档-' + name
        rows.append({
            'queueId': qid,
            'oldName': name,
            'newName': new_name,
            'timeEnabled': bool(info.get('TimeEnabled')),
            'startupEnabled': bool(info.get('StartUpEnabled')),
        })
    safe_print('QUEUE ARCHIVE PLAN（只读预览）')
    for r in rows:
        safe_print('  %-36s -> %-40s time=%s' % (r['oldName'], r['newName'], r['timeEnabled']))
    if not rows:
        safe_print('没有需要归档的临时队列')
        return 0
    if not apply:
        safe_print('dry-run：未写 MAS。加 --apply 才执行归档。')
        return 0

    ts = dt.datetime.now().strftime('%Y%m%d_%H%M%S')
    backup_dir = os.path.join(BASE, 'backups', 'queue_archive_' + ts)
    os.makedirs(backup_dir, exist_ok=True)
    if os.path.exists(CONFIG):
        shutil.copy2(CONFIG, os.path.join(backup_dir, 'QueueConfig.json'))
    with io.open(os.path.join(backup_dir, 'queues_api.json'), 'w', encoding='utf-8') as f:
        json.dump(qs, f, ensure_ascii=False, indent=2)

    for r in rows:
        d = queue_data(api, r['queueId'])
        info = d.get('Info') or {}
        info['Name'] = r['newName']
        info['TimeEnabled'] = False
        info['StartUpEnabled'] = False
        d['Info'] = info
        resp = post_json(api, '/api/queue/update', {'queueId': r['queueId'], 'data': d})
        if resp.get('code') not in (200, None) or resp.get('status') == 'error':
            raise SystemExit('归档失败 %s：%s' % (r['oldName'], json.dumps(resp, ensure_ascii=False)))
        safe_print('  archived %-36s -> %s' % (r['oldName'], r['newName']))

    mapping = {
        'stage': 'archived',
        'createdAt': dt.datetime.now().isoformat(timespec='seconds'),
        'backupDir': backup_dir,
        'queues': rows,
    }
    os.makedirs(os.path.dirname(out_path) or '.', exist_ok=True)
    with io.open(out_path, 'w', encoding='utf-8') as f:
        json.dump(mapping, f, ensure_ascii=False, indent=2)
        f.write('\n')
    safe_print('mapping: %s' % out_path)
    safe_print('backup: %s' % backup_dir)
    return 0


def restore(api: str, mapping_path: str, apply: bool) -> int:
    with io.open(mapping_path, 'r', encoding='utf-8-sig') as f:
        mapping = json.load(f)
    rows = mapping.get('queues') or []
    safe_print('QUEUE RESTORE PLAN')
    for r in rows:
        safe_print('  %-40s -> %s' % (r['newName'], r['oldName']))
    if not apply:
        safe_print('dry-run：未写 MAS。加 --apply 才恢复。')
        return 0
    for r in rows:
        d = queue_data(api, r['queueId'])
        info = d.get('Info') or {}
        info['Name'] = r['oldName']
        info['TimeEnabled'] = False
        d['Info'] = info
        resp = post_json(api, '/api/queue/update', {'queueId': r['queueId'], 'data': d})
        if resp.get('code') not in (200, None) or resp.get('status') == 'error':
            raise SystemExit('恢复失败 %s：%s' % (r['newName'], json.dumps(resp, ensure_ascii=False)))
        safe_print('  restored %s' % r['oldName'])
    return 0


def main():
    ap = argparse.ArgumentParser(description='MAS 队列清理 Apply（默认 dry-run）')
    ap.add_argument('--stage', choices=['archive', 'restore'], required=True)
    ap.add_argument('--mapping', default='dist/mas_queue_cleanup.json')
    ap.add_argument('--mas-api', default=os.environ.get('MAS_API', 'http://127.0.0.1:36163'))
    ap.add_argument('--apply', action='store_true')
    args = ap.parse_args()
    if args.stage == 'archive':
        return archive(args.mas_api, args.mapping, args.apply)
    return restore(args.mas_api, args.mapping, args.apply)


if __name__ == '__main__':
    raise SystemExit(main())
