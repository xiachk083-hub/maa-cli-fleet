#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""只读采集：模拟器/MAA 的 CPU 曲线，用于 emulator session 画像。

不写 MAS、不改队列、不杀进程。输出 JSONL + 汇总 JSON。

用法（机端）：
  python tools/session_profiler.py --minutes 10 --interval 5 `
    --mumu "E:/MuMu Player 12/nx_main/MuMuManager.exe" `
    --out dist/session_profile.jsonl `
    --summary dist/session_profile_summary.json
"""
from __future__ import annotations

import argparse
import datetime as dt
import io
import json
import os
import statistics
import subprocess
import sys
import time

try:
    import psutil
except Exception as e:  # pragma: no cover
    raise SystemExit('需要 psutil：%s' % e)


def pct(vals, p):
    if not vals:
        return 0.0
    vals = sorted(vals)
    k = max(0, min(len(vals) - 1, int(round((p / 100.0) * (len(vals) - 1)))))
    return round(vals[k], 1)


def mumu_instances(mgr: str) -> dict:
    try:
        out = subprocess.check_output([mgr, 'info', '-v', 'all'], text=True,
                                      encoding='utf-8', errors='replace', timeout=30)
        return json.loads(out)
    except Exception:
        return {}


def main():
    ap = argparse.ArgumentParser(description='模拟器/MAA CPU 曲线采集（只读）')
    ap.add_argument('--minutes', type=float, default=10)
    ap.add_argument('--interval', type=float, default=5)
    ap.add_argument('--mumu', default=r'E:\MuMu Player 12\nx_main\MuMuManager.exe')
    ap.add_argument('--out', default='dist/session_profile.jsonl')
    ap.add_argument('--summary', default='dist/session_profile_summary.json')
    ap.add_argument('--label', default='')
    args = ap.parse_args()

    os.makedirs(os.path.dirname(args.out) or '.', exist_ok=True)
    os.makedirs(os.path.dirname(args.summary) or '.', exist_ok=True)
    start = time.time()
    end = start + max(1.0, args.minutes * 60)
    psutil.cpu_percent(None)
    proc_cache = {}
    inst_cache = {}
    last_mumu = 0.0
    rows = []

    def proc_cpu(pid, kind):
        key = (kind, int(pid))
        p = proc_cache.get(key)
        if p is None:
            try:
                p = psutil.Process(int(pid))
                p.cpu_percent(None)
                proc_cache[key] = p
            except Exception:
                return None
            return None
        try:
            return round(p.cpu_percent(None), 1)
        except Exception:
            proc_cache.pop(key, None)
            return None

    f = io.open(args.out, 'w', encoding='utf-8')
    try:
        while time.time() < end:
            now = dt.datetime.now()
            cpu = round(psutil.cpu_percent(interval=0.2), 1)
            vm = psutil.virtual_memory()
            if time.time() - last_mumu >= 15:
                inst_cache = mumu_instances(args.mumu)
                last_mumu = time.time()
            emus = []
            for idx, info in (inst_cache or {}).items():
                if not info.get('is_process_started'):
                    continue
                pid = info.get('pid')
                hpid = info.get('headless_pid')
                c1 = proc_cpu(pid, 'muMuPid') if pid else None
                c2 = proc_cpu(hpid, 'muMuHeadless') if hpid else None
                emus.append({
                    'index': str(idx),
                    'name': info.get('name', ''),
                    'adb_port': info.get('adb_port', ''),
                    'pid': pid,
                    'headless_pid': hpid,
                    'cpu_pid': c1,
                    'cpu_headless': c2,
                })
            maa = []
            for p in psutil.process_iter(['pid', 'name', 'exe']):
                try:
                    name = (p.info.get('name') or '').lower()
                    if name != 'maa.exe':
                        continue
                    c = proc_cpu(p.info['pid'], 'maa')
                    maa.append({'pid': p.info['pid'], 'path': p.info.get('exe') or '', 'cpu': c})
                except Exception:
                    continue
            maa.sort(key=lambda x: x.get('path', ''))
            row = {
                'ts': now.isoformat(timespec='seconds'),
                'label': args.label,
                'cpu': cpu,
                'mem_free_mb': round(vm.available / 1024 / 1024),
                'mem_used_pct': vm.percent,
                'emulators': emus,
                'emu_count': len(emus),
                'maa': maa,
                'maa_count': len(maa),
            }
            rows.append(row)
            f.write(json.dumps(row, ensure_ascii=False) + '\n')
            f.flush()
            time.sleep(max(0.2, args.interval - 0.2))
    except KeyboardInterrupt:
        pass
    finally:
        f.close()

    def group(field):
        g = {}
        for r in rows:
            g.setdefault(r[field], []).append(r)
        out = {}
        for k, rs in sorted(g.items()):
            cpus = [x['cpu'] for x in rs]
            per_emu = []
            for x in rs:
                if x['emu_count']:
                    per_emu.append(x['cpu'] / x['emu_count'])
            out[str(k)] = {
                'samples': len(rs),
                'cpu_mean': round(statistics.mean(cpus), 1),
                'cpu_p95': pct(cpus, 95),
                'cpu_max': round(max(cpus), 1),
                'cpu_per_emu_mean': round(statistics.mean(per_emu), 1) if per_emu else 0,
                'maa_mean': round(statistics.mean([x['maa_count'] for x in rs]), 1),
                'mem_free_mb_mean': round(statistics.mean([x['mem_free_mb'] for x in rs])),
            }
        return out

    summary = {
        'label': args.label,
        'startedAt': dt.datetime.fromtimestamp(start).isoformat(timespec='seconds'),
        'endedAt': dt.datetime.now().isoformat(timespec='seconds'),
        'minutes': round((time.time() - start) / 60.0, 1),
        'samples': len(rows),
        'by_emu_count': group('emu_count'),
        'by_maa_count': group('maa_count'),
        'last': rows[-1] if rows else None,
    }
    with io.open(args.summary, 'w', encoding='utf-8') as sf:
        json.dump(summary, sf, ensure_ascii=False, indent=2)
        sf.write('\n')
    print(json.dumps(summary, ensure_ascii=False, indent=2))


if __name__ == '__main__':
    main()
