#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MAS 多用户脚本拆分 Plan（只读，不写 MAS）。

目标：一个账号一个脚本、一个用户。对每个多用户脚本：
  * 保留一个用户在原脚本（默认第一个启用用户，可用 --keep 指定主号）；
  * 其余启用用户各计划一个单用户克隆脚本；
  * 禁用用户只列出，不自动迁移；
  * 新脚本名、目标队列、需要人工确认的点全部输出。

红线：只调用 /api/scripts/get、/api/scripts/user/get、/api/queue/get、/api/queue/item/get；
不调用 add/update/delete/dispatch。
"""
from __future__ import annotations

import argparse
import datetime as dt
import io
import json
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mas_inventory import collect_mas, collect_queues, norm, safe_print  # noqa: E402


def slug(text: str) -> str:
    s = re.sub(r'[^\w\u4e00-\u9fff.-]+', '_', str(text or '')).strip('._-')
    return (s[:40] or 'user')


def load_keep(path: str) -> dict:
    """keep 文件：
    {"scripts": {"<scriptId>": "<userId>"}}
    或 {"scripts": [{"scriptId": "...", "userId": "..."}]}
    """
    if not path:
        return {}
    with io.open(path, 'r', encoding='utf-8-sig') as f:
        data = json.load(f)
    raw = data.get('scripts') if isinstance(data, dict) else data
    out = {}
    if isinstance(raw, dict):
        for sid, uid in raw.items():
            out[str(sid)] = str(uid)
    elif isinstance(raw, list):
        for item in raw:
            if isinstance(item, dict) and item.get('scriptId') and item.get('userId'):
                out[str(item['scriptId'])] = str(item['userId'])
    return out


def parse_keep_args(items, scripts, users) -> dict:
    """--keep '<scriptId|scriptName>:<userId|userName>'。"""
    out = {}
    for item in items or []:
        script_key, sep, user_key = str(item).partition(':')
        if not sep:
            raise SystemExit('--keep 格式：<scriptId|scriptName>:<userId|userName>')
        script = None
        if any(s['scriptId'] == script_key for s in scripts):
            script = next(s for s in scripts if s['scriptId'] == script_key)
        else:
            hits = [s for s in scripts if norm(s.get('name')) == norm(script_key)]
            if len(hits) != 1:
                raise SystemExit('--keep 脚本不唯一或不存在：%s' % script_key)
            script = hits[0]
        us = [u for u in users if u.get('scriptId') == script['scriptId']]
        if any(u['userId'] == user_key for u in us):
            user = next(u for u in us if u['userId'] == user_key)
        else:
            hits = [u for u in us if norm(u.get('name')) == norm(user_key)]
            if len(hits) != 1:
                raise SystemExit('--keep 用户不唯一或不存在：%s / %s' % (script_key, user_key))
            user = hits[0]
        out[script['scriptId']] = user['userId']
    return out


def queue_names_for(sid: str, queues) -> list:
    out = []
    for q in queues:
        if sid in q.get('scripts', []):
            out.append({
                'queueId': q.get('queueId', ''),
                'name': q.get('name', ''),
                'timeEnabled': bool(q.get('timeEnabled')),
                'startupEnabled': bool(q.get('startupEnabled')),
            })
    return out


def new_script_name(base: str, user: dict, used: set, planned: set) -> str:
    stem = slug(base or 'script')
    label = slug(user.get('name') or user.get('userId', '')[:8])
    name = '%s-%s' % (stem, label)
    if name in used or name in planned:
        name = '%s-%s-%s' % (stem, label, user.get('userId', '')[:6])
    i = 2
    while name in used or name in planned:
        name = '%s-%s-%s-%d' % (stem, label, user.get('userId', '')[:4], i)
        i += 1
    planned.add(name)
    return name


def main():
    ap = argparse.ArgumentParser(description='MAS 多用户脚本拆分 Plan（只读）')
    ap.add_argument('--mas-api', default=os.environ.get('MAS_API', 'http://127.0.0.1:36163'))
    ap.add_argument('--out', default='dist/mas_split_plan.json')
    ap.add_argument('--keep-out', default='dist/mas_split_keep.template.json')
    ap.add_argument('--keep', action='append', default=[],
                    help='保留在原脚本的用户：<scriptId|scriptName>:<userId|userName>，可重复')
    ap.add_argument('--keep-file', default='', help='保留映射 JSON')
    args = ap.parse_args()

    scripts, users = collect_mas(args.mas_api)
    queues = collect_queues(args.mas_api)
    keep_map = load_keep(args.keep_file)
    keep_map.update(parse_keep_args(args.keep, scripts, users))

    by_script = {}
    for u in users:
        by_script.setdefault(u.get('scriptId', ''), []).append(u)

    used_names = {s.get('name', '') for s in scripts}
    planned_names = set()
    items = []
    total_active = total_clones = total_disabled = 0

    for s in scripts:
        sid = s['scriptId']
        us = by_script.get(sid, [])
        active = [u for u in us if u.get('status') and u.get('remainedDay') != 0]
        disabled = [u for u in us if not (u.get('status') and u.get('remainedDay') != 0)]
        if len(active) <= 1:
            continue
        total_active += len(active)
        total_disabled += len(disabled)
        qs = queue_names_for(sid, queues)
        keep_uid = keep_map.get(sid, '')
        keep = next((u for u in active if u['userId'] == keep_uid), None)
        keep_source = 'explicit' if keep else 'default'
        if keep is None:
            keep = active[0]
            keep_source = 'default'
        clones = []
        for u in active:
            if u['userId'] == keep['userId']:
                continue
            name = new_script_name(s.get('name', ''), u, used_names, planned_names)
            clones.append({
                'oldUserId': u['userId'],
                'userName': u.get('name', ''),
                'newScriptName': name,
                'newScriptId': '(apply 时生成)',
                'newUserId': '(clone 时由 MAS 生成)',
                'queueTargets': [q['name'] for q in qs if q.get('timeEnabled')] or [q['name'] for q in qs],
            })
        warnings = []
        if keep_source == 'default':
            warnings.append('保留用户是默认第一个启用用户；请确认主号或用 --keep 指定')
        if disabled:
            warnings.append('脚本内还有 %d 个禁用用户；迁移前先导出/归档，不要静默删除' % len(disabled))
        if any('并行' in q.get('name', '') for q in qs):
            warnings.append('队列名含“并行”；同一 MAA/模拟器上并发前先确认并发度/脚本锁')
        items.append({
            'scriptId': sid,
            'scriptName': s.get('name', ''),
            'emulatorIndex': s.get('emulatorIndex', ''),
            'maaPath': s.get('maaPath', ''),
            'queueInfo': qs,
            'activeUsers': len(active),
            'keep': {
                'userId': keep['userId'],
                'userName': keep.get('name', ''),
                'source': keep_source,
            },
            'clones': clones,
            'disabledUsers': [
                {'userId': u['userId'], 'userName': u.get('name', '')}
                for u in disabled
            ],
            'warnings': warnings,
        })

    total_clones = sum(len(x['clones']) for x in items)
    total_disabled = sum(len(x['disabledUsers']) for x in items)
    out = {
        'generatedAt': dt.datetime.now().isoformat(timespec='seconds'),
        'mode': 'plan-only',
        'masApi': args.mas_api,
        'summary': {
            'sharedScripts': len(items),
            'activeUsersInShared': total_active,
            'clones': total_clones,
            'newScripts': total_clones,
            'disabledUsers': total_disabled,
            'needsReview': sum(1 for x in items if x['warnings']),
        },
        'items': items,
    }
    os.makedirs(os.path.dirname(args.out) or '.', exist_ok=True)
    with io.open(args.out, 'w', encoding='utf-8') as f:
        json.dump(out, f, ensure_ascii=False, indent=2)
        f.write('\n')

    keep_template = {
        '_说明': '把 keepUserId 改成要留在原脚本的主号；其余启用用户会计划拆成单用户脚本。',
        'scripts': [
            {
                'scriptId': x['scriptId'],
                'scriptName': x['scriptName'],
                'keepUserId': x['keep']['userId'],
                'keepUserName': x['keep']['userName'],
                'candidates': [
                    {'userId': x['keep']['userId'], 'userName': x['keep']['userName']}
                ] + [
                    {'userId': c['oldUserId'], 'userName': c['userName']}
                    for c in x['clones']
                ],
            }
            for x in items
        ],
    }
    os.makedirs(os.path.dirname(args.keep_out) or '.', exist_ok=True)
    with io.open(args.keep_out, 'w', encoding='utf-8') as f:
        json.dump(keep_template, f, ensure_ascii=False, indent=2)
        f.write('\n')

    s = out['summary']
    safe_print('MAS SPLIT PLAN（只读，不会写 MAS）')
    safe_print('共享脚本=%d 启用用户=%d 计划克隆=%d 禁用记录=%d' % (
        s['sharedScripts'], s['activeUsersInShared'], s['clones'], s['disabledUsers']))
    for x in items:
        mark = ' [需确认]' if x['warnings'] else ''
        safe_print('  %s (%s, emu=%s): 保留 %s [%s]%s -> 克隆 %d' % (
            x['scriptName'], x['scriptId'][:8], x['emulatorIndex'],
            x['keep']['userName'], x['keep']['source'], mark, len(x['clones'])))
        for c in x['clones']:
            safe_print('      -> %-24s user=%s queue=%s' % (
                c['newScriptName'], c['userName'], ','.join(c['queueTargets']) or '-'))
        for w in x['warnings']:
            safe_print('      ! ' + w)
    safe_print('plan json: %s' % args.out)
    safe_print('keep template: %s' % args.keep_out)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
