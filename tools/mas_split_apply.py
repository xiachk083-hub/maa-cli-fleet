#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MAS 多用户脚本拆分 Apply（分阶段，默认 dry-run）。

stage:
  clone    备份 -> 暂停队列 -> 为每个启用用户克隆单用户脚本并改名；
           旧脚本/旧用户/旧队列项不动，新脚本暂不进队列。
  finalize 验证新脚本 -> 删旧队列项 -> 加新队列项 -> 删旧脚本里的启用用户；
           默认仍暂停队列，--enable-queue 才恢复。
  rollback 删除本工具创建的新脚本，恢复队列 TimeEnabled；旧脚本不动。

只操作 plan 里指定的一个脚本；Apply 必须显式 --apply。
"""
from __future__ import annotations

import argparse
import datetime as dt
import io
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from mas_inventory import collect_mas, collect_queues, norm, safe_print  # noqa: E402

BASE = r'E:\AUTO-MAS'
CONFIG_DIR = os.path.join(BASE, 'config')
DATA_DIR = os.path.join(BASE, 'data')


def api(api_base: str, path: str, body: dict) -> dict:
    req = urllib.request.Request(
        api_base.rstrip('/') + path,
        data=json.dumps(body).encode('utf-8'),
        headers={'Content-Type': 'application/json'},
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            return json.load(r)
    except Exception as e:
        raise RuntimeError('%s%s：%s' % (api_base, path, e))


def ok(resp: dict, what: str) -> dict:
    if resp.get('code') not in (200, None) or resp.get('status') == 'error':
        raise RuntimeError('%s 失败：%s' % (what, json.dumps(resp, ensure_ascii=False)))
    return resp


def find_plan_item(plan: dict, target: str) -> dict:
    items = plan.get('items') or []
    for x in items:
        if x.get('scriptId') == target or norm(x.get('scriptName')) == norm(target):
            return x
    raise SystemExit('Plan 里找不到脚本：%s' % target)


def get_users(api_base, script_id) -> dict:
    data = ok(api(api_base, '/api/scripts/user/get', {'scriptId': script_id, 'userId': None}),
              'scripts/user/get').get('data') or {}
    return {uid: cfg for uid, cfg in data.items() if uid != 'instances' and isinstance(cfg, dict)}


def get_script(api_base, script_id) -> dict:
    data = ok(api(api_base, '/api/scripts/get', {'scriptId': script_id}), 'scripts/get').get('data') or {}
    return data.get(script_id) or {}


def queue_data(api_base, queue_id) -> dict:
    data = ok(api(api_base, '/api/queue/get', {'queueId': queue_id}), 'queue/get').get('data') or {}
    return data.get(queue_id) or {}


def queue_items(api_base, queue_id) -> dict:
    data = ok(api(api_base, '/api/queue/item/get', {'queueId': queue_id}), 'queue/item/get').get('data') or {}
    return {iid: it for iid, it in data.items() if iid != 'instances' and isinstance(it, dict)}


def set_queue_time(api_base, queue_id, enabled: bool) -> None:
    q = queue_data(api_base, queue_id)
    info = q.get('Info') or {}
    info['TimeEnabled'] = bool(enabled)
    q['Info'] = info
    ok(api(api_base, '/api/queue/update', {'queueId': queue_id, 'data': q}), 'queue/update')


def backup(base: str, backup_dir: str, sid: str, api_base: str) -> None:
    os.makedirs(backup_dir, exist_ok=True)
    cfg_backup = os.path.join(backup_dir, 'config')
    os.makedirs(cfg_backup, exist_ok=True)
    for name in ('ScriptConfig.json', 'QueueConfig.json'):
        src = os.path.join(CONFIG_DIR, name)
        if os.path.exists(src):
            shutil.copy2(src, os.path.join(cfg_backup, name))
    src_data = os.path.join(DATA_DIR, sid)
    if os.path.exists(src_data):
        shutil.copytree(src_data, os.path.join(backup_dir, 'data', sid), dirs_exist_ok=True)
    # API 视图也留一份
    scripts = ok(api(api_base, '/api/scripts/get', {}), 'scripts/get').get('data') or {}
    queues = ok(api(api_base, '/api/queue/get', {}), 'queue/get').get('data') or {}
    with io.open(os.path.join(backup_dir, 'scripts_api.json'), 'w', encoding='utf-8') as f:
        json.dump(scripts, f, ensure_ascii=False, indent=2)
    with io.open(os.path.join(backup_dir, 'queues_api.json'), 'w', encoding='utf-8') as f:
        json.dump(queues, f, ensure_ascii=False, indent=2)


def match_new_user(new_users: dict, old_info: dict) -> str:
    target_id = str(old_info.get('Id') or '').strip()
    target_name = norm(old_info.get('Name') or '')
    target_server = str(old_info.get('Server') or '')
    hits = []
    for uid, cfg in new_users.items():
        info = cfg.get('Info') or {}
        if target_id and str(info.get('Id') or '') == target_id:
            hits.append(uid)
            continue
        if target_name and norm(info.get('Name') or '') == target_name and str(info.get('Server') or '') == target_server:
            hits.append(uid)
    if len(hits) != 1:
        raise RuntimeError('克隆里目标用户匹配不唯一：id=%s name=%s hits=%d' % (target_id, target_name, len(hits)))
    return hits[0]


def clone_stage(args, plan: dict) -> int:
    item = find_plan_item(plan, args.script)
    old_sid = item['scriptId']
    old_sname = item.get('scriptName', '')
    clones = item.get('clones') or []
    if not clones:
        raise SystemExit('Plan 没有 clones')
    old_users = get_users(args.mas_api, old_sid)
    old_active = {uid: cfg for uid, cfg in old_users.items()
                  if (cfg.get('Info') or {}).get('Status') and (cfg.get('Info') or {}).get('RemainedDay') != 0}
    if len(old_active) != len(clones):
        raise SystemExit('旧脚本启用用户 %d != Plan clones %d，拒绝执行' % (len(old_active), len(clones)))
    qis = item.get('queueInfo') or []
    if len(qis) != 1:
        raise SystemExit('目前只支持单队列脚本；实际队列数 %d' % len(qis))
    qid = qis[0]['queueId']
    q_prev = bool((queue_data(args.mas_api, qid).get('Info') or {}).get('TimeEnabled'))
    ts = dt.datetime.now().strftime('%Y%m%d_%H%M%S')
    backup_dir = args.backup_dir or os.path.join(BASE, 'backups', 'split_' + ts)
    out_path = args.out or os.path.join(args.fleet_root, 'dist', 'mas_split_apply_%s.json' % old_sid[:8])

    safe_print('CLONE PLAN  %s (%s) -> %d 个单用户脚本' % (old_sname, old_sid, len(clones)))
    safe_print('queue=%s previousTimeEnabled=%s' % (qis[0]['name'], q_prev))
    safe_print('backup=%s' % backup_dir)
    for c in clones:
        safe_print('  -> %-24s user=%s' % (c['newScriptName'], c['userName']))
    if not args.apply:
        safe_print('dry-run：未写 MAS。加 --apply 才执行 clone。')
        return 0

    backup(BASE, backup_dir, old_sid, args.mas_api)
    set_queue_time(args.mas_api, qid, False)
    safe_print('已暂停队列：%s' % qis[0]['name'])

    created = []
    try:
        for c in clones:
            target_old_uid = c['oldUserId']
            old_cfg = old_active.get(target_old_uid)
            if not old_cfg:
                raise RuntimeError('Plan 目标用户不在旧脚本启用列表：%s' % target_old_uid)
            old_info = old_cfg.get('Info') or {}
            resp = ok(api(args.mas_api, '/api/scripts/add', {'type': 'MAA', 'scriptId': old_sid}),
                      'scripts/add clone')
            new_sid = str(resp.get('scriptId') or '')
            if not new_sid:
                raise RuntimeError('克隆没有返回 scriptId')
            created.append(new_sid)
            new_users = get_users(args.mas_api, new_sid)
            target_new_uid = match_new_user(new_users, old_info)
            for uid in list(new_users.keys()):
                if uid == target_new_uid:
                    continue
                ok(api(args.mas_api, '/api/scripts/user/delete', {'scriptId': new_sid, 'userId': uid}),
                   'scripts/user/delete')
            # 改脚本名，保留 Emulator/Run
            sd = get_script(args.mas_api, new_sid)
            sd.setdefault('Info', {})['Name'] = c['newScriptName']
            ok(api(args.mas_api, '/api/scripts/update', {'scriptId': new_sid, 'data': sd}),
               'scripts/update name')
            c['newScriptId'] = new_sid
            c['newUserId'] = target_new_uid
            c['newScriptName'] = c['newScriptName']
            safe_print('  cloned %-24s script=%s user=%s' % (
                c['newScriptName'], new_sid[:8], target_new_uid[:8]))
    except Exception as e:
        safe_print('ERROR：%s' % e)
        safe_print('回滚本次已创建的新脚本…')
        for sid in created:
            try:
                api(args.mas_api, '/api/scripts/delete', {'scriptId': sid})
            except Exception:
                pass
        set_queue_time(args.mas_api, qid, q_prev)
        safe_print('已回滚新脚本，队列恢复 TimeEnabled=%s；旧脚本未动' % q_prev)
        return 2

    mapping = {
        'stage': 'cloned',
        'createdAt': dt.datetime.now().isoformat(timespec='seconds'),
        'mode': plan.get('mode', 'plan-only-full'),
        'oldScriptId': old_sid,
        'oldScriptName': old_sname,
        'queueId': qid,
        'queueName': qis[0].get('name', ''),
        'previousTimeEnabled': q_prev,
        'backupDir': backup_dir,
        'newScripts': [
            {
                'oldUserId': c['oldUserId'],
                'userName': c['userName'],
                'server': c.get('server', ''),
                'scriptId': c['newScriptId'],
                'userId': c['newUserId'],
                'scriptName': c['newScriptName'],
            }
            for c in clones
        ],
    }
    os.makedirs(os.path.dirname(out_path) or '.', exist_ok=True)
    with io.open(out_path, 'w', encoding='utf-8') as f:
        json.dump(mapping, f, ensure_ascii=False, indent=2)
        f.write('\n')
    safe_print('mapping: %s' % out_path)
    safe_print('旧脚本/旧用户/旧队列项未动；新脚本未进队列，队列仍暂停')
    return 0


def finalize_stage(args, mapping: dict) -> int:
    old_sid = mapping['oldScriptId']
    qid = mapping['queueId']
    new_scripts = mapping.get('newScripts') or []
    if not new_scripts:
        raise SystemExit('mapping 没有 newScripts')
    for ns in new_scripts:
        users = get_users(args.mas_api, ns['scriptId'])
        if list(users.keys()) != [ns['userId']]:
            raise SystemExit('新脚本 %s 不是单用户：%s' % (ns['scriptName'], list(users.keys())))
    safe_print('FINALIZE %s -> %d 个单用户脚本' % (mapping['oldScriptName'], len(new_scripts)))
    if not args.apply:
        safe_print('dry-run：未写 MAS。加 --apply 才执行 finalize。')
        return 0

    # 删旧队列项（只删指向旧脚本的）
    for iid, it in queue_items(args.mas_api, qid).items():
        if str((it.get('Info') or {}).get('ScriptId') or '') == old_sid:
            ok(api(args.mas_api, '/api/queue/item/delete', {'queueId': qid, 'queueItemId': iid}),
               'queue/item/delete old')
            safe_print('  removed old queue item %s' % iid[:8])
    # 加新队列项
    for ns in new_scripts:
        r = ok(api(args.mas_api, '/api/queue/item/add', {'queueId': qid}), 'queue/item/add')
        iid = str(r.get('queueItemId') or '')
        ok(api(args.mas_api, '/api/queue/item/update', {
            'queueId': qid, 'queueItemId': iid, 'data': {'Info': {'ScriptId': ns['scriptId']}}
        }), 'queue/item/update new')
        safe_print('  added queue item %-24s %s' % (ns['scriptName'], iid[:8]))
    # 清旧脚本里的启用用户；禁用记录留作归档
    old_users = get_users(args.mas_api, old_sid)
    for uid, cfg in old_users.items():
        info = cfg.get('Info') or {}
        if not (info.get('Status') and info.get('RemainedDay') != 0):
            continue
        ok(api(args.mas_api, '/api/scripts/user/delete', {'scriptId': old_sid, 'userId': uid}),
           'scripts/user/delete old')
        safe_print('  removed old user %s' % uid[:8])
    if args.enable_queue:
        set_queue_time(args.mas_api, qid, bool(mapping.get('previousTimeEnabled', True)))
        safe_print('队列已恢复 TimeEnabled=%s' % mapping.get('previousTimeEnabled', True))
    else:
        safe_print('队列仍暂停（确认并发/顺序后再 --enable-queue）')
    mapping['stage'] = 'finalized'
    mapping['finalizedAt'] = dt.datetime.now().isoformat(timespec='seconds')
    return 0


def rollback_stage(args, mapping: dict) -> int:
    qid = mapping.get('queueId')
    safe_print('ROLLBACK 删除本工具创建的新脚本，旧脚本不动')
    if not args.apply:
        safe_print('dry-run：未写 MAS。加 --apply 才执行 rollback。')
        return 0
    for ns in mapping.get('newScripts') or []:
        try:
            ok(api(args.mas_api, '/api/scripts/delete', {'scriptId': ns['scriptId']}), 'scripts/delete')
            safe_print('  deleted %s' % ns['scriptName'])
        except Exception as e:
            safe_print('  delete failed %s: %s' % (ns['scriptName'], e))
    if qid:
        try:
            set_queue_time(args.mas_api, qid, bool(mapping.get('previousTimeEnabled', True)))
        except Exception as e:
            safe_print('  queue restore failed: %s' % e)
    mapping['stage'] = 'rolled_back'
    return 0


def save_mapping(path: str, mapping: dict) -> None:
    os.makedirs(os.path.dirname(path) or '.', exist_ok=True)
    with io.open(path, 'w', encoding='utf-8') as f:
        json.dump(mapping, f, ensure_ascii=False, indent=2)
        f.write('\n')


def main():
    ap = argparse.ArgumentParser(description='MAS 多用户脚本拆分 Apply（分阶段）')
    ap.add_argument('--stage', choices=['clone', 'finalize', 'rollback'], required=True)
    ap.add_argument('--plan', default='dist/mas_split_plan.json')
    ap.add_argument('--script', default='', help='脚本名或 scriptId（clone 必填）')
    ap.add_argument('--mapping', default='', help='clone 输出的 mapping（finalize/rollback 必填）')
    ap.add_argument('--mas-api', default=os.environ.get('MAS_API', 'http://127.0.0.1:36163'))
    ap.add_argument('--fleet-root', default=os.path.abspath(os.path.join(os.path.dirname(__file__), '..')))
    ap.add_argument('--out', default='')
    ap.add_argument('--backup-dir', default='')
    ap.add_argument('--enable-queue', action='store_true')
    ap.add_argument('--apply', action='store_true')
    args = ap.parse_args()

    if args.stage == 'clone':
        if not args.script:
            raise SystemExit('--stage clone 需要 --script')
        with io.open(args.plan, 'r', encoding='utf-8-sig') as f:
            plan = json.load(f)
        return clone_stage(args, plan)
    if not args.mapping:
        raise SystemExit('--stage %s 需要 --mapping' % args.stage)
    with io.open(args.mapping, 'r', encoding='utf-8-sig') as f:
        mapping = json.load(f)
    if args.stage == 'finalize':
        rc = finalize_stage(args, mapping)
    else:
        rc = rollback_stage(args, mapping)
    if args.apply and rc == 0:
        save_mapping(args.mapping, mapping)
    return rc


if __name__ == '__main__':
    raise SystemExit(main())
