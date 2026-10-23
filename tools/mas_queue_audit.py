#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MAS 队列清理 Plan（只读，不写 MAS）。

输出：
  * 正式队列（并行*）建议保留；
  * 临时队列（临时-*）建议归档；
  * 同一 MAA 路径被多个队列占用；
  * 同一脚本出现在多个队列；
  * 队列内多脚本共用同一 MAA 路径时的串行/冷却要求。
"""
from __future__ import annotations

import argparse
import collections
import datetime as dt
import io
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mas_inventory import post_json, safe_print  # noqa: E402


def norm_path(s: str) -> str:
    return str(s or '').strip().replace('/', '\\').rstrip('\\').lower()


def classify(name: str) -> str:
    n = str(name or '')
    if n.startswith('并行'):
        return 'official'
    if n.startswith('归档-'):
        return 'archived'
    if '临时' in n:
        return 'temporary'
    return 'other'


def main():
    ap = argparse.ArgumentParser(description='MAS 队列清理 Plan（只读）')
    ap.add_argument('--mas-api', default=os.environ.get('MAS_API', 'http://127.0.0.1:36163'))
    ap.add_argument('--out', default='dist/mas_queue_plan.json')
    ap.add_argument('--md', default='dist/mas_queue_plan.md')
    args = ap.parse_args()

    scripts_raw = post_json(args.mas_api, '/api/scripts/get', {}).get('data') or {}
    sm = {}
    for sid, sc in scripts_raw.items():
        info = sc.get('Info') or {}
        emu = sc.get('Emulator') or {}
        sm[sid] = {
            'scriptId': sid,
            'name': info.get('Name', ''),
            'maaPath': info.get('Path', ''),
            'emulatorIndex': str(emu.get('Index', '')),
        }
    qraw = post_json(args.mas_api, '/api/queue/get', {}).get('data') or {}
    queues = []
    for qid, q in qraw.items():
        info = q.get('Info') or {}
        items = post_json(args.mas_api, '/api/queue/item/get', {'queueId': qid}).get('data') or {}
        scripts = []
        for iid, it in items.items():
            if iid == 'instances' or not isinstance(it, dict):
                continue
            sid = (it.get('Info') or {}).get('ScriptId')
            if sid and sid != '-':
                scripts.append(sm.get(sid, {'scriptId': sid, 'name': '', 'maaPath': '', 'emulatorIndex': ''}))
        queues.append({
            'queueId': qid,
            'name': info.get('Name', ''),
            'timeEnabled': bool(info.get('TimeEnabled')),
            'startupEnabled': bool(info.get('StartUpEnabled')),
            'class': classify(info.get('Name', '')),
            'scripts': scripts,
        })

    # MAA path -> queues
    path_q = collections.defaultdict(list)
    for q in queues:
        for s in q['scripts']:
            path_q[norm_path(s.get('maaPath'))].append(q)
    path_bindings = {}
    for path, qs in path_q.items():
        uniq = {}
        for q in qs:
            uniq[q['queueId']] = q
        qs = list(uniq.values())
        off = [q for q in qs if q['class'] == 'official']
        tmp = [q for q in qs if q['class'] == 'temporary']
        arc = [q for q in qs if q['class'] == 'archived']
        if not off and not tmp and not arc:
            continue
        severity = 'HIGH' if (off and tmp) or len(off) > 1 else 'MEDIUM'
        path_bindings[path] = {
            'maaPath': path,
            'official': [q['name'] for q in off],
            'temporary': [q['name'] for q in tmp],
            'archived': [q['name'] for q in arc],
            'queues': [{'queueId': q['queueId'], 'name': q['name'], 'timeEnabled': q['timeEnabled']} for q in qs],
            'severity': severity,
        }

    # duplicate scripts across queues
    sq = collections.defaultdict(list)
    for q in queues:
        for s in q['scripts']:
            sq[s['scriptId']].append(q)
    duplicates = []
    for sid, qs in sq.items():
        uniq = {}
        for q in qs:
            uniq[q['queueId']] = q
        qs = list(uniq.values())
        if len(qs) > 1:
            enabled = [q for q in qs if q['timeEnabled']]
            if len(enabled) > 1:
                sev = 'HIGH'
            elif len(enabled) == 1 and len(qs) > 1:
                sev = 'LOW'
            else:
                sev = 'MEDIUM'
            duplicates.append({
                'scriptId': sid,
                'name': sm.get(sid, {}).get('name', ''),
                'queues': [{'queueId': q['queueId'], 'name': q['name'], 'timeEnabled': q['timeEnabled']} for q in qs],
                'severity': sev,
            })

    for q in queues:
        reasons = []
        if q['class'] == 'official':
            action = 'KEEP'
        elif q['class'] == 'temporary':
            action = 'ARCHIVE'
            reasons.append('临时补跑/验收队列，不参与正式调度；保留配置但归档')
        elif q['class'] == 'archived':
            action = 'ARCHIVED'
            reasons.append('已归档；保持 TimeEnabled=false，不参与调度')
        else:
            action = 'REVIEW'
        if len(q['scripts']) > 1:
            reasons.append('队列内 %d 个脚本共用同一个 MAA 路径，必须串行并留冷却' % len(q['scripts']))
        paths = {norm_path(s.get('maaPath')) for s in q['scripts'] if s.get('maaPath')}
        if len(paths) > 1:
            reasons.append('队列内使用多个 MAA 路径（MAA 池）')
        q['action'] = action
        q['pool'] = len(paths) > 1
        q['reasons'] = reasons
        q['maaPaths'] = sorted(paths)

    recs = []
    for q in queues:
        if q['action'] == 'ARCHIVE':
            recs.append('归档临时队列：%s (%s)' % (q['name'], q['queueId'][:8]))
    for path, b in sorted(path_bindings.items()):
        if b['severity'] == 'HIGH':
            recs.append('MAA 路径冲突：%s -> official=%s, temporary=%s' % (
                path, ', '.join(b['official']) or '-', ', '.join(b['temporary']) or '-'))
    for d in duplicates:
        if d['severity'] in ('HIGH', 'MEDIUM'):
            recs.append('重复脚本：%s 出现在 %s' % (d['name'], ', '.join(x['name'] for x in d['queues'])))
    recs.append('正式队列一个 MAA 路径只保留一个；临时队列全部 TimeEnabled=false 并归档')
    recs.append('同一队列内脚本串行，MAA 进程退出后至少冷却 5-10 秒再启动下一个')

    out = {
        'generatedAt': dt.datetime.now().isoformat(timespec='seconds'),
        'mode': 'plan-only',
        'masApi': args.mas_api,
        'summary': {
            'queues': len(queues),
            'official': sum(1 for q in queues if q['class'] == 'official'),
            'temporary': sum(1 for q in queues if q['class'] == 'temporary'),
            'archived': sum(1 for q in queues if q['class'] == 'archived'),
            'other': sum(1 for q in queues if q['class'] == 'other'),
            'enabled': sum(1 for q in queues if q['timeEnabled']),
            'pathConflicts': sum(1 for b in path_bindings.values() if b['severity'] == 'HIGH'),
            'duplicateScripts': len(duplicates),
        },
        'pathBindings': path_bindings,
        'duplicateScripts': duplicates,
        'queues': queues,
        'recommendations': recs,
    }
    os.makedirs(os.path.dirname(args.out) or '.', exist_ok=True)
    with io.open(args.out, 'w', encoding='utf-8') as f:
        json.dump(out, f, ensure_ascii=False, indent=2)
        f.write('\n')

    md = ['# MAS 队列清理 Plan（只读）', '',
          '生成：%s' % out['generatedAt'], '',
          '| 队列 | 类型 | 启用 | 脚本数 | MAA | 动作 |', '|---|---|---:|---:|---|---|']
    for q in queues:
        md.append('| %s | %s | %s | %d | %s | %s |' % (
            q['name'], q['class'], 'yes' if q['timeEnabled'] else 'no',
            len(q['scripts']), ', '.join(q['maaPaths']) or '-', q['action']))
    md += ['', '## 冲突', '']
    for path, b in sorted(path_bindings.items()):
        if b['severity'] == 'HIGH':
            md.append('- `%s`：official=%s；temporary=%s' % (
                path, ', '.join(b['official']) or '-', ', '.join(b['temporary']) or '-'))
    md += ['', '## 重复脚本', '']
    for d in duplicates:
        md.append('- %s：%s' % (d['name'], ', '.join(x['name'] for x in d['queues'])))
    md += ['', '## 建议', ''] + ['- %s' % x for x in recs]
    os.makedirs(os.path.dirname(args.md) or '.', exist_ok=True)
    with io.open(args.md, 'w', encoding='utf-8') as f:
        f.write('\n'.join(md) + '\n')

    s = out['summary']
    safe_print('MAS QUEUE PLAN（只读，不写 MAS）')
    safe_print('queues=%d official=%d temporary=%d archived=%d enabled=%d pathConflicts=%d duplicateScripts=%d' % (
        s['queues'], s['official'], s['temporary'], s['archived'], s['enabled'], s['pathConflicts'], s['duplicateScripts']))
    for q in queues:
        safe_print('  %-28s %-10s enabled=%-5s scripts=%d maa=%s action=%s' % (
            q['name'], q['class'], q['timeEnabled'], len(q['scripts']),
            ','.join(q['maaPaths']) or '-', q['action']))
    safe_print('json: %s' % args.out)
    safe_print('md: %s' % args.md)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
