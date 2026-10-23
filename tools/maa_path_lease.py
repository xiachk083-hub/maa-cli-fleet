#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MAA 路径租约 + 冷却（只读检查/本地租约文件，不改 MAS）。

用途：
  * 调度器在启动某个 MAA 目录前，先 wait/acquire，保证旧 MAA 已退出；
  * 同一路径同一时间只允许一个 owner；
  * 任务结束后 release，记录冷却起点。

命令：
  python tools/maa_path_lease.py status
  python tools/maa_path_lease.py wait --path "E:/MAA-Multi/MAA_3" --cooldown 10
  python tools/maa_path_lease.py acquire --path ... --owner job-a19
  python tools/maa_path_lease.py release --path ... --owner job-a19
"""
from __future__ import annotations

import argparse
import datetime as dt
import io
import json
import os
import sys
import time

try:
    import psutil
except Exception as e:  # pragma: no cover
    raise SystemExit('需要 psutil：%s' % e)


def norm_path(s: str) -> str:
    return str(s or '').strip().replace('/', '\\').rstrip('\\').lower()


def load_state(path: str) -> dict:
    if not os.path.exists(path):
        return {'leases': {}}
    try:
        with io.open(path, 'r', encoding='utf-8-sig') as f:
            d = json.load(f)
        d.setdefault('leases', {})
        return d
    except Exception:
        return {'leases': {}}


def save_state(path: str, d: dict) -> None:
    os.makedirs(os.path.dirname(path) or '.', exist_ok=True)
    tmp = path + '.tmp'
    with io.open(tmp, 'w', encoding='utf-8') as f:
        json.dump(d, f, ensure_ascii=False, indent=2)
        f.write('\n')
    os.replace(tmp, path)


def maa_procs(path: str) -> list:
    target = norm_path(path)
    out = []
    for p in psutil.process_iter(['pid', 'name', 'exe', 'create_time']):
        try:
            if (p.info.get('name') or '').lower() != 'maa.exe':
                continue
            exe = norm_path(p.info.get('exe') or '')
            if exe != target:
                continue
            out.append({'pid': p.info['pid'], 'exe': p.info.get('exe'), 'started': p.info.get('create_time')})
        except Exception:
            continue
    return out


def wait_free(path: str, state_path: str, cooldown: float, timeout: float) -> bool:
    target = norm_path(path)
    deadline = time.time() + max(1, timeout)
    while time.time() < deadline:
        ps = maa_procs(path)
        if not ps:
            break
        time.sleep(2)
    else:
        return False
    d = load_state(state_path)
    ent = (d.get('leases') or {}).get(target) or {}
    last = float(ent.get('lastRelease') or 0)
    remain = cooldown - (time.time() - last)
    if remain > 0:
        time.sleep(remain)
    return True


def acquire(path: str, owner: str, state_path: str, cooldown: float, timeout: float) -> bool:
    target = norm_path(path)
    d = load_state(state_path)
    ent = (d.get('leases') or {}).get(target) or {}
    old_owner = ent.get('owner')
    old_pid = int(ent.get('pid') or 0)
    if old_owner and old_owner != owner:
        alive = False
        try:
            alive = psutil.pid_exists(old_pid) and psutil.Process(old_pid).is_running()
        except Exception:
            alive = False
        if alive:
            print('BLOCKED: %s 已被 owner=%s pid=%s 持有' % (path, old_owner, old_pid))
            return False
    if not wait_free(path, state_path, cooldown, timeout):
        print('BLOCKED: %s 仍有 MAA 进程或冷却超时' % path)
        return False
    d.setdefault('leases', {})[target] = {
        'path': path,
        'owner': owner,
        'pid': os.getpid(),
        'acquiredAt': dt.datetime.now().isoformat(timespec='seconds'),
        'lastRelease': ent.get('lastRelease', 0),
    }
    save_state(state_path, d)
    print('LEASE %s owner=%s' % (path, owner))
    return True


def release(path: str, owner: str, state_path: str) -> bool:
    target = norm_path(path)
    d = load_state(state_path)
    ent = (d.get('leases') or {}).get(target) or {}
    if ent.get('owner') and ent.get('owner') != owner:
        print('BLOCKED: owner 不匹配：%s != %s' % (ent.get('owner'), owner))
        return False
    ent.update({'lastRelease': time.time(), 'releasedAt': dt.datetime.now().isoformat(timespec='seconds')})
    ent.pop('owner', None)
    ent.pop('pid', None)
    d.setdefault('leases', {})[target] = ent
    save_state(state_path, d)
    print('RELEASE %s' % path)
    return True


def status(state_path: str) -> int:
    d = load_state(state_path)
    seen = {}
    for p in psutil.process_iter(['pid', 'name', 'exe']):
        try:
            if (p.info.get('name') or '').lower() != 'maa.exe':
                continue
            exe = p.info.get('exe') or ''
            seen.setdefault(norm_path(exe), []).append({'pid': p.info['pid'], 'exe': exe})
        except Exception:
            continue
    for path, ps in sorted(seen.items()):
        ent = (d.get('leases') or {}).get(path) or {}
        print('%s  procs=%s  owner=%s  lastRelease=%s' % (
            path, [x['pid'] for x in ps], ent.get('owner') or '-', ent.get('lastRelease') or '-'))
    if not seen:
        print('没有运行中的 MAA 进程')
    return 0


def main():
    ap = argparse.ArgumentParser(description='MAA 路径租约 + 冷却')
    ap.add_argument('--state', default='dist/maa_path_leases.json')
    sub = ap.add_subparsers(dest='cmd', required=True)
    sub.add_parser('status')
    for name in ('wait', 'acquire', 'release'):
        p = sub.add_parser(name)
        p.add_argument('--path', required=True)
        p.add_argument('--owner', default='fleet')
        p.add_argument('--cooldown', type=float, default=10)
        p.add_argument('--timeout', type=float, default=300)
    args = ap.parse_args()
    if args.cmd == 'status':
        return status(args.state)
    if args.cmd == 'wait':
        ok = wait_free(args.path, args.state, args.cooldown, args.timeout)
        print('READY %s' % args.path if ok else 'TIMEOUT %s' % args.path)
        return 0 if ok else 1
    if args.cmd == 'acquire':
        return 0 if acquire(args.path, args.owner, args.state, args.cooldown, args.timeout) else 1
    return 0 if release(args.path, args.owner, args.state) else 1


if __name__ == '__main__':
    raise SystemExit(main())
