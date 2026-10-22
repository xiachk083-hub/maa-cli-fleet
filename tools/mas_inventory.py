#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""MAS 只读盘点 / 接管 Plan（不写 MAS、不 dispatch）。

用途：
  * inventory：同时看 MAS 账号 + fleet 账号，回答“谁在谁手里、今天清了没有”；
  * plan：给一份 desired JSON，输出 CREATE_USER / CREATE_SCRIPT /
          UPDATE_USER / UPDATE_SCRIPT / NOOP / BLOCKED，只做计划不落地。

红线：
  * 只调用 MAS 的查询接口（/api/scripts/get、/api/scripts/user/get）；
  * 不调用 add/update/dispatch；
  * 手机号/邮箱只输出掩码，不输出密码。
"""
from __future__ import annotations

import argparse
import datetime as dt
import html
import io
import json
import os
import re
import sys
import urllib.error
import urllib.request

DEFAULT_API = os.environ.get('MAS_API', 'http://127.0.0.1:36163')
TASK_FLAGS = [
    ('IfStartUp', 'StartUp'),
    ('IfRecruit', 'Recruit'),
    ('IfInfrast', 'Infrast'),
    ('IfFight', 'Fight'),
    ('IfMall', 'Mall'),
    ('IfAward', 'Award'),
    ('IfRoguelike', 'Roguelike'),
    ('IfReclamation', 'Reclamation'),
]


def safe_print(s: str) -> None:
    try:
        print(s)
    except UnicodeEncodeError:
        print(s.encode('ascii', 'replace').decode('ascii'))


def load_json(path: str) -> dict:
    with io.open(path, 'r', encoding='utf-8-sig') as f:
        return json.load(f)


def post_json(api: str, path: str, body: dict) -> dict:
    req = urllib.request.Request(
        api.rstrip('/') + path,
        data=json.dumps(body).encode('utf-8'),
        headers={'Content-Type': 'application/json'},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.load(r)
    except Exception as e:
        raise SystemExit('MAS API 请求失败：%s%s：%s' % (api, path, e))


def mask_id(value: str) -> str:
    s = str(value or '').strip()
    if not s:
        return ''
    if re.fullmatch(r'1[3-9]\d{9}', s):
        return s[:3] + '****' + s[-4:]
    if '@' in s:
        local, _, domain = s.partition('@')
        return (local[:2] + '***@' + domain) if local else '***@' + domain
    if len(s) <= 4:
        return '***'
    return s[:2] + '***' + s[-2:]


def norm(s: str) -> str:
    return re.sub(r'\s+', '', str(s or '')).strip().lower()


def game_day(now: dt.datetime | None = None) -> str:
    now = now or dt.datetime.now()
    return (now - dt.timedelta(hours=4)).strftime('%Y-%m-%d')


def mas_today(last_proxy: str, today: str) -> bool:
    s = str(last_proxy or '').strip()
    return s.startswith(today)


def collect_history(root: str):
    """扫描 AUTO-MAS history：{账号名: {latest: 'YYYY-MM-DD', days: [...]}}。"""
    if not root or not os.path.isdir(root):
        return {}
    out = {}
    try:
        day_names = sorted(os.listdir(root), reverse=True)
    except Exception:
        return {}
    for day in day_names:
        dp = os.path.join(root, day)
        if not os.path.isdir(dp) or not re.fullmatch(r'\d{4}-\d{2}-\d{2}', day):
            continue
        try:
            names = os.listdir(dp)
        except Exception:
            continue
        for name in names:
            p = os.path.join(dp, name)
            if not os.path.isdir(p):
                continue
            try:
                files = [f for f in os.listdir(p) if f.lower().endswith(('.log', '.json'))]
            except Exception:
                continue
            if not files:
                continue
            ent = out.setdefault(name, {'latest': day, 'days': []})
            ent['days'].append(day)
    return out


def history_for(hist: dict, name: str) -> dict:
    if not name:
        return {}
    if name in hist:
        return hist[name]
    n = norm(name)
    for k, v in hist.items():
        if norm(k) == n:
            return v
    return {}


def collect_mas(api: str):
    scripts_raw = post_json(api, '/api/scripts/get', {}).get('data') or {}
    scripts, users = [], []
    for sid, sc in scripts_raw.items():
        info = sc.get('Info') or {}
        emu = sc.get('Emulator') or {}
        if not emu:
            continue  # 只看 MAA 脚本；其他类型不参与 fleet 对账
        script = {
            'scriptId': sid,
            'name': info.get('Name', ''),
            'maaPath': info.get('Path', ''),
            'emulatorId': emu.get('Id', ''),
            'emulatorIndex': str(emu.get('Index', '')),
            'run': sc.get('Run') or {},
        }
        scripts.append(script)
        uraw = post_json(api, '/api/scripts/user/get', {'scriptId': sid, 'userId': None}).get('data') or {}
        for uid, ui in uraw.items():
            if uid == 'instances' or not isinstance(ui, dict):
                continue
            inf = ui.get('Info') or {}
            dat = ui.get('Data') or {}
            task = ui.get('Task') or {}
            users.append({
                'scriptId': sid,
                'userId': uid,
                'scriptName': script['name'],
                'emulatorIndex': script['emulatorIndex'],
                'maaPath': script['maaPath'],
                'name': inf.get('Name', ''),
                'accountId': inf.get('Id', ''),
                'accountIdMasked': mask_id(inf.get('Id', '')),
                'server': inf.get('Server', ''),
                'stage': inf.get('Stage', ''),
                'status': bool(inf.get('Status', False)),
                'remainedDay': inf.get('RemainedDay'),
                'annihilation': inf.get('Annihilation', ''),
                'lastProxyDate': dat.get('LastProxyDate', ''),
                'proxyTimes': dat.get('ProxyTimes'),
                'tasks': {label: bool(task.get(key, False)) for key, label in TASK_FLAGS},
            })
    return scripts, users


def collect_fleet(root: str):
    acc_path = os.path.join(root, 'runner', 'accounts.json')
    state_path = os.path.join(root, 'runner', 'state.json')
    if not os.path.exists(acc_path):
        raise SystemExit('找不到 fleet 账号表：%s' % acc_path)
    accounts = load_json(acc_path).get('accounts') or []
    tasks = {}
    if os.path.exists(state_path):
        tasks = load_json(state_path).get('tasks') or {}
    day = game_day()
    for a in accounts:
        d = tasks.get('daily:' + str(a.get('id', '')), {}) or {}
        r = tasks.get('rogue:' + str(a.get('id', '')), {}) or {}
        a['_daily'] = d
        a['_rogue'] = r
        a['_today'] = (d.get('state') == 'done' and str(d.get('day', '')) == day)
    return accounts, tasks, day


def collect_queues(api: str):
    """读取 MAS 队列及队列项，返回 [{queueId,name,timeEnabled,startupEnabled,scripts}]。"""
    qraw = post_json(api, '/api/queue/get', {}).get('data') or {}
    out = []
    for qid, qc in qraw.items():
        info = (qc or {}).get('Info') or {}
        items = post_json(api, '/api/queue/item/get', {'queueId': qid}).get('data') or {}
        scripts = []
        for iid, it in items.items():
            if iid == 'instances' or not isinstance(it, dict):
                continue
            sid = (it.get('Info') or {}).get('ScriptId')
            if sid and sid != '-':
                scripts.append(sid)
        out.append({
            'queueId': qid,
            'name': info.get('Name', ''),
            'timeEnabled': bool(info.get('TimeEnabled')),
            'startupEnabled': bool(info.get('StartUpEnabled')),
            'scripts': scripts,
        })
    return out


def build_inventory(accounts, scripts, users, day, hist=None, queues=None):
    hist = hist or {}
    queues = queues or []
    queue_by_script = {}
    for q in queues:
        for sid in q.get('scripts', []):
            queue_by_script.setdefault(sid, []).append(q)
    fleet_by_emu = {}
    for a in accounts:
        fleet_by_emu.setdefault(str(a.get('emu', '')), []).append(a)
    mas_by_emu = {}
    for u in users:
        mas_by_emu.setdefault(str(u.get('emulatorIndex', '')), []).append(u)

    rows = []
    used_users = set()

    for a in accounts:
        emu = str(a.get('emu', ''))
        name_hits = [x for x in users if norm(x.get('name')) == norm(a.get('name'))]
        emu_hits = mas_by_emu.get(emu, [])
        match = 'none'
        u = None
        if len(name_hits) == 1:
            u, match = name_hits[0], 'name'
        elif len(emu_hits) == 1:
            u, match = emu_hits[0], 'emu'
        elif len(emu_hits) > 1:
            nm = [x for x in emu_hits if norm(x.get('name')) == norm(a.get('name'))]
            if len(nm) == 1:
                u, match = nm[0], 'emu+name'
            else:
                match = 'ambiguous'
        elif len(name_hits) > 1:
            match = 'ambiguous'
        if u:
            used_users.add(u['userId'])

        fleet_enabled = bool(a.get('enabled'))
        mas_present = u is not None
        owner = 'BOTH' if fleet_enabled and mas_present else 'FLEET' if fleet_enabled else 'MAS' if mas_present else 'NONE'
        fleet_today = bool(a.get('_today'))
        h = history_for(hist, (u or {}).get('name', '')) if u else {}
        mas_history_today = bool(h) and day in h.get('days', [])
        mas_history_latest = h.get('latest', '')
        qs = queue_by_script.get((u or {}).get('scriptId', ''), []) if u else []
        mas_auto_queued = any(q.get('timeEnabled') for q in qs)
        mas_queues = ', '.join(q.get('name', '') + ('*' if q.get('timeEnabled') else '') for q in qs)
        mas_day = mas_history_today or bool(u and mas_today(u.get('lastProxyDate'), day))
        fleet_active = fleet_enabled and bool(a.get('_daily'))
        mas_active = bool(u and (u or {}).get('status'))
        active = fleet_active or mas_active
        cleared = fleet_today or mas_day
        today_state = 'cleared' if cleared else ('not_cleared' if active else 'unknown')
        conflict = bool(fleet_enabled and mas_present and (u or {}).get('status'))
        rows.append({
            'key': a.get('id', ''),
            'name': a.get('name', ''),
            'owner': owner,
            'fleetEnabled': fleet_enabled,
            'fleetActive': fleet_active,
            'fleetDaily': (a.get('_daily') or {}).get('state', ''),
            'fleetDay': (a.get('_daily') or {}).get('day', ''),
            'fleetToday': fleet_today,
            'fleetTodayCell': ('已清' if fleet_today else '未清') if fleet_active else '-',
            'fleetNext': (a.get('_daily') or {}).get('nextDueAt', ''),
            'fleetSanity': '%s/%s' % ((a.get('_daily') or {}).get('sanityCur', ''), (a.get('_daily') or {}).get('sanityMax', '')),
            'fleetNote': (a.get('_daily') or {}).get('note', ''),
            'fleetOut': (a.get('_daily') or {}).get('outFile', ''),
            'rogue': (a.get('_rogue') or {}).get('state', ''),
            'emu': emu,
            'masUserId': (u or {}).get('userId', ''),
            'masName': (u or {}).get('name', ''),
            'masId': (u or {}).get('accountIdMasked', ''),
            'masServer': (u or {}).get('server', ''),
            'masStage': (u or {}).get('stage', ''),
            'masStatus': (u or {}).get('status'),
            'masActive': mas_active,
            'masToday': mas_day,
            'masTodayCell': ('已清' if mas_day else '未清') if mas_active else '-',
            'masLastProxy': (u or {}).get('lastProxyDate', '') or mas_history_latest,
            'masHistoryToday': mas_history_today,
            'masHistoryLatest': mas_history_latest,
            'masHistoryDays': len(h.get('days', [])),
            'masQueues': mas_queues,
            'masAutoQueued': mas_auto_queued,
            'masTimes': (u or {}).get('proxyTimes'),
            'active': active,
            'match': match,
            'conflict': conflict,
            'cleared': cleared,
            'todayState': today_state,
        })

    for u in users:
        if u['userId'] in used_users:
            continue
        h = history_for(hist, u.get('name', ''))
        mas_history_today = bool(h) and day in h.get('days', [])
        mas_history_latest = h.get('latest', '')
        qs = queue_by_script.get(u.get('scriptId', ''), [])
        mas_auto_queued = any(q.get('timeEnabled') for q in qs)
        mas_queues = ', '.join(q.get('name', '') + ('*' if q.get('timeEnabled') else '') for q in qs)
        mas_day = mas_history_today or mas_today(u.get('lastProxyDate'), day)
        mas_active = bool(u.get('status'))
        rows.append({
            'key': '',
            'name': u.get('name', ''),
            'owner': 'MAS',
            'fleetEnabled': False,
            'fleetActive': False,
            'fleetDaily': '',
            'fleetDay': '',
            'fleetToday': False,
            'fleetTodayCell': '-',
            'fleetNext': '',
            'fleetSanity': '',
            'fleetNote': '',
            'fleetOut': '',
            'rogue': '',
            'emu': u.get('emulatorIndex', ''),
            'masUserId': u.get('userId', ''),
            'masName': u.get('name', ''),
            'masId': u.get('accountIdMasked', ''),
            'masServer': u.get('server', ''),
            'masStage': u.get('stage', ''),
            'masStatus': u.get('status'),
            'masActive': mas_active,
            'masToday': mas_day,
            'masTodayCell': ('已清' if mas_day else '未清') if mas_active else '-',
            'masLastProxy': u.get('lastProxyDate', '') or mas_history_latest,
            'masHistoryToday': mas_history_today,
            'masHistoryLatest': mas_history_latest,
            'masHistoryDays': len(h.get('days', [])),
            'masQueues': mas_queues,
            'masAutoQueued': mas_auto_queued,
            'masTimes': u.get('proxyTimes'),
            'active': mas_active,
            'match': 'none',
            'conflict': False,
            'cleared': mas_day,
            'todayState': 'cleared' if mas_day else ('not_cleared' if mas_active else 'unknown'),
        })

    summary = {
        'fleetAccounts': len(accounts),
        'fleetEnabled': sum(1 for a in accounts if a.get('enabled')),
        'fleetDisabled': sum(1 for a in accounts if not a.get('enabled')),
        'masUsers': len(users),
        'masMatched': len(used_users),
        'masUnmatched': len(users) - len(used_users),
        'masAutoQueued': sum(1 for u in users if any(q.get('timeEnabled') for q in queue_by_script.get(u.get('scriptId', ''), []))),
        'masNotAutoQueued': sum(1 for u in users if not any(q.get('timeEnabled') for q in queue_by_script.get(u.get('scriptId', ''), []))),
        'both': sum(1 for r in rows if r['owner'] == 'BOTH'),
        'masOnly': sum(1 for r in rows if r['owner'] == 'MAS'),
        'fleetOnly': sum(1 for r in rows if r['owner'] == 'FLEET'),
        'neither': sum(1 for r in rows if r['owner'] == 'NONE'),
        'conflicts': sum(1 for r in rows if r['conflict']),
        'todayCleared': sum(1 for r in rows if r['todayState'] == 'cleared'),
        'todayNotCleared': sum(1 for r in rows if r['todayState'] == 'not_cleared'),
        'todayUnknown': sum(1 for r in rows if r['todayState'] == 'unknown'),
    }
    return rows, summary


def render_html(data: dict) -> str:
    rows = data['rows']
    s = data['summary']
    css = """
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Microsoft YaHei',sans-serif;margin:0;background:#0f1115;color:#e6e8ee}
.wrap{max-width:1500px;margin:0 auto;padding:24px}
h1{font-size:22px;margin:0 0 6px} .sub{color:#8b93a7;font-size:13px;margin-bottom:18px}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(130px,1fr));gap:10px;margin-bottom:18px}
.card{background:#171b23;border:1px solid #252b38;border-radius:10px;padding:12px}
.card .n{font-size:24px;font-weight:700}.card .l{color:#8b93a7;font-size:12px;margin-top:2px}
.bar{display:flex;gap:8px;flex-wrap:wrap;margin:12px 0}
button{background:#202635;color:#dfe4ef;border:1px solid #30384a;border-radius:8px;padding:7px 12px;cursor:pointer}
button:hover{background:#2a3243}.hint{color:#8b93a7;font-size:12px}
table{width:100%;border-collapse:collapse;font-size:12.5px}
th,td{padding:7px 8px;border-bottom:1px solid #222936;text-align:left;vertical-align:top;white-space:nowrap}
th{position:sticky;top:0;background:#171b23;color:#aab2c5;z-index:1}
tr:hover{background:#151a23}
.b{display:inline-block;padding:2px 7px;border-radius:999px;font-size:11px;font-weight:600}
.FLEET{background:#16351f;color:#7fe0a2}.MAS{background:#162a44;color:#79b8ff}
.BOTH{background:#3a2415;color:#ffb86b}.NONE{background:#262a33;color:#9aa3b5}
.yes{background:#16351f;color:#7fe0a2}.no{background:#3a1b1b;color:#ff9b9b}.unk{background:#262a33;color:#9aa3b5}
.conf{color:#ff8a8a;font-weight:700}.muted{color:#76809a}
footer{margin-top:18px;color:#76809a;font-size:12px}
@media(max-width:900px){th,td{white-space:normal}}
"""
    cards = [
        ('FLEET 账号', s['fleetAccounts']),
        ('FLEET 启用', s['fleetEnabled']),
        ('MAS 用户', s['masUsers']),
        ('匹配到 fleet', s['masMatched']),
        ('未匹配 MAS', s['masUnmatched']),
        ('BOTH', s['both']),
        ('冲突', s['conflicts']),
        ('今天已清', s['todayCleared']),
        ('今天未清', s['todayNotCleared']),
        ('未知', s['todayUnknown']),
        ('MAS 自动队列', s['masAutoQueued']),
    ]
    out = ['<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">',
           '<meta name="viewport" content="width=device-width,initial-scale=1">',
           '<title>MAS / FLEET 只读盘点</title><style>%s</style></head><body><div class="wrap">' % css,
           '<h1>MAS / FLEET 只读盘点</h1>',
           '<div class="sub">生成 %s · 游戏日 %s · MAS %s · FLEET %s</div>' % (
               html.escape(data['generatedAt']), html.escape(data['gameDay']),
               html.escape(data['masApi']), html.escape(data['fleetRoot'])),
           '<div class="cards">']
    for label, n in cards:
        out.append('<div class="card"><div class="n">%s</div><div class="l">%s</div></div>' % (n, html.escape(label)))
    out.append('</div>')
    out.append('<div class="bar">'
               '<button data-filter="all">全部</button>'
               '<button data-filter="notcleared">今天没清</button>'
               '<button data-filter="conflict">冲突</button>'
               '<button data-filter="MAS">MAS</button>'
               '<button data-filter="FLEET">FLEET</button>'
               '<button data-filter="BOTH">BOTH</button>'
               '<span class="hint">只读：不写 MAS、不 dispatch</span></div>')
    out.append('<table id="t"><thead><tr>'
               '<th>账号</th><th>名称</th><th>归属</th><th>fleet 启用</th><th>fleet 日常</th>'
               '<th>fleet 今日</th><th>MAS 状态</th><th>MAS 队列</th><th>MAS 今日</th><th>MAS 最近 history</th>'
               '<th>模拟器</th><th>匹配</th><th>冲突</th><th>备注</th>'
               '</tr></thead><tbody>')
    for r in rows:
        owner = r['owner']
        cleared = {'cleared': 'yes', 'not_cleared': 'no', 'unknown': 'unknown'}.get(r['todayState'], 'unknown')
        mas_status = 'ON' if r['masStatus'] else ('OFF' if r['masStatus'] is not None else '')
        out.append('<tr data-owner="%s" data-cleared="%s" data-conflict="%s">' % (
            owner, cleared, '1' if r['conflict'] else '0'))
        out.append('<td>%s</td><td>%s</td>' % (html.escape(r['key']), html.escape(r['name'])))
        out.append('<td><span class="b %s">%s</span></td>' % (owner, owner))
        out.append('<td>%s</td><td>%s</td>' % ('是' if r['fleetEnabled'] else '否', html.escape(r['fleetDaily'] or '-')))
        fcell = r['fleetTodayCell']
        fcls = 'yes' if fcell == '已清' else ('no' if fcell == '未清' else 'unk')
        out.append('<td><span class="b %s">%s</span></td>' % (fcls, fcell))
        out.append('<td>%s</td>' % html.escape(mas_status or '-'))
        out.append('<td class="muted">%s</td>' % html.escape(r.get('masQueues') or '-'))
        mcell = r['masTodayCell']
        mcls = 'yes' if mcell == '已清' else ('no' if mcell == '未清' else 'unk')
        out.append('<td><span class="b %s">%s</span></td>' % (mcls, mcell))
        hist_day = r.get('masHistoryLatest') or r.get('masLastProxy') or '-'
        out.append('<td>%s / %s天</td>' % (html.escape(hist_day), html.escape(str(r.get('masHistoryDays') or 0))))
        out.append('<td>%s</td><td>%s</td>' % (html.escape(r['emu'] or '-'), html.escape(r['match'])))
        out.append('<td>%s</td>' % ('<span class="conf">冲突</span>' if r['conflict'] else '-'))
        note = r['fleetNote'] or r['masId'] or ''
        out.append('<td class="muted">%s</td>' % html.escape(note))
        out.append('</tr>')
    out.append('</tbody></table>')
    out.append('<footer>只读盘点；手机号/邮箱已掩码。Plan 模式只输出计划，不写 MAS。</footer>')
    out.append('</div><script>'
               'const rows=[...document.querySelectorAll("#t tbody tr")];'
               'document.querySelectorAll("[data-filter]").forEach(b=>b.onclick=()=>{'
               'const f=b.dataset.filter;document.querySelectorAll("[data-filter]").forEach(x=>x.style.background="");'
               'b.style.background="#2a3243";'
               'rows.forEach(r=>{let ok=f==="all"||r.dataset.owner===f||'
               '(f==="notcleared"&&r.dataset.cleared==="no")||(f==="conflict"&&r.dataset.conflict==="1");'
               'r.style.display=ok?"":"none";});});'
               '</script></body></html>')
    return ''.join(out)


def plan_desired(desired_path: str, scripts, users):
    data = load_json(desired_path)
    specs = data.get('accounts') if isinstance(data, dict) else data
    if not isinstance(specs, list):
        raise SystemExit('desired 文件格式：{"accounts":[...]}')
    plan = []
    for spec in specs:
        key = str(spec.get('key') or spec.get('id') or '')
        name = str(spec.get('name') or '')
        account_id = str(spec.get('accountId') or '')
        server = str(spec.get('server') or '')
        stage = str(spec.get('stage') or '')
        emu = str(spec.get('emulatorIndex') or spec.get('emu') or '')
        maa = str(spec.get('maaPath') or '')
        want_tasks = spec.get('tasks') or {}
        changes = []
        action = 'NOOP'
        reason = ''
        script = None
        existing_user = None
        matches = []

        # 1) 账号是否已存在：优先 accountId，其次 name（跨所有脚本）
        if account_id and '*' not in account_id and '<' not in account_id:
            matches = [u for u in users if u.get('accountId') == account_id]
        if not matches and name:
            matches = [u for u in users if norm(u.get('name')) == norm(name)]
        if len(matches) > 1:
            action, reason = 'BLOCKED', '账号匹配到多个 MAS 用户'
        elif len(matches) == 1:
            existing_user = matches[0]
            script = next((s for s in scripts if s['scriptId'] == existing_user['scriptId']), None)
            if script is None:
                action, reason = 'BLOCKED', '账号所在脚本不是 MAA 脚本'
            elif emu and str(script.get('emulatorIndex')) != emu:
                action, reason = 'BLOCKED', '账号已存在于另一个脚本（模拟器 %s）' % script.get('emulatorIndex')
            elif maa and norm(script.get('maaPath')) != norm(maa):
                changes.append('script.maaPath')

        # 2) 账号不存在：按模拟器/MAA 找脚本，找不到则计划新建
        if action != 'BLOCKED' and existing_user is None:
            cands = [s for s in scripts if emu and str(s.get('emulatorIndex')) == emu]
            if not cands and maa:
                cands = [s for s in scripts if norm(s.get('maaPath')) == norm(maa)]
            if len(cands) > 1 and maa:
                cands = [s for s in cands if norm(s.get('maaPath')) == norm(maa)]
            if len(cands) > 1:
                action, reason = 'BLOCKED', '模拟器/MAA 对应多个脚本'
            elif len(cands) == 0:
                if not emu or not maa:
                    action, reason = 'BLOCKED', '创建脚本需要 emulatorIndex + maaPath'
                else:
                    action = 'CREATE_SCRIPT'
            else:
                script = cands[0]

        # 3) 已有账号：字段级 diff；新账号：只做 CREATE 计划
        if action != 'BLOCKED':
            if existing_user:
                u = existing_user
                if server and server != u.get('server'):
                    changes.append('user.server')
                if stage and stage != u.get('stage'):
                    changes.append('user.stage')
                for k, label in TASK_FLAGS:
                    if k in want_tasks:
                        want = want_tasks[k]
                    elif label in want_tasks:
                        want = want_tasks[label]
                    else:
                        continue
                    if bool(want) != bool(u.get('tasks', {}).get(label)):
                        changes.append('user.' + label)
                if changes:
                    has_script = any(x.startswith('script.') for x in changes)
                    has_user = any(x.startswith('user.') for x in changes)
                    action = 'UPDATE_SCRIPT+USER' if has_script and has_user else 'UPDATE_SCRIPT' if has_script else 'UPDATE_USER'
                else:
                    action = 'NOOP'
            else:
                if not account_id or '*' in account_id or '<' in account_id:
                    action, reason = 'BLOCKED', '创建账号需要可用的 accountId/凭据'
                elif action == 'CREATE_SCRIPT':
                    action = 'CREATE_SCRIPT+USER'
                else:
                    action = 'CREATE_USER'

        plan.append({
            'key': key,
            'name': name,
            'action': action,
            'scriptId': (script or {}).get('scriptId', ''),
            'userId': (existing_user or {}).get('userId', ''),
            'changes': changes,
            'reason': reason,
        })
    return plan


def main():
    ap = argparse.ArgumentParser(description='MAS 只读盘点 / 接管 Plan')
    ap.add_argument('--mas-api', default=DEFAULT_API)
    ap.add_argument('--mas-history', default=os.environ.get('MAS_HISTORY', r'E:\AUTO-MAS\history'))
    ap.add_argument('--fleet-root', default=os.path.abspath(os.path.join(os.path.dirname(__file__), '..')))
    ap.add_argument('--out', default='dist/mas_inventory.html')
    ap.add_argument('--json', default='dist/mas_inventory.json')
    ap.add_argument('--desired', default='', help='Plan 用的 desired JSON（只读，不写 MAS）')
    ap.add_argument('--plan-out', default='dist/mas_plan.json')
    ap.add_argument('--print', action='store_true', help='在终端打印简表')
    args = ap.parse_args()

    scripts, users = collect_mas(args.mas_api)
    accounts, tasks, day = collect_fleet(args.fleet_root)
    hist = collect_history(args.mas_history)
    queues = collect_queues(args.mas_api)

    if args.desired:
        plan = plan_desired(args.desired, scripts, users)
        out = {'generatedAt': dt.datetime.now().isoformat(timespec='seconds'), 'gameDay': day, 'masHistory': os.path.abspath(args.mas_history), 'plan': plan}
        os.makedirs(os.path.dirname(args.plan_out) or '.', exist_ok=True)
        with io.open(args.plan_out, 'w', encoding='utf-8') as f:
            json.dump(out, f, ensure_ascii=False, indent=2)
            f.write('\n')
        safe_print('PLAN（只读，不会写 MAS）')
        for p in plan:
            safe_print('  %-8s %-16s %s %s' % (p['key'], p['action'], p['scriptId'], ','.join(p['changes']) or p['reason']))
        safe_print('plan json: %s' % args.plan_out)
        return

    rows, summary = build_inventory(accounts, scripts, users, day, hist, queues)
    out = {
        'generatedAt': dt.datetime.now().isoformat(timespec='seconds'),
        'gameDay': day,
        'masApi': args.mas_api,
        'masHistory': os.path.abspath(args.mas_history),
        'fleetRoot': os.path.abspath(args.fleet_root),
        'summary': summary,
        'masScripts': scripts,
        'masQueues': queues,
        'rows': rows,
    }
    os.makedirs(os.path.dirname(args.json) or '.', exist_ok=True)
    with io.open(args.json, 'w', encoding='utf-8') as f:
        json.dump(out, f, ensure_ascii=False, indent=2)
        f.write('\n')
    os.makedirs(os.path.dirname(args.out) or '.', exist_ok=True)
    with io.open(args.out, 'w', encoding='utf-8') as f:
        f.write(render_html(out))

    safe_print('MAS/FLEET inventory: %s' % args.out)
    safe_print('JSON: %s' % args.json)
    safe_print('summary: fleet=%d enabled=%d mas=%d matched=%d unmatched=%d autoQueued=%d notAutoQueued=%d both=%d masOnly=%d fleetOnly=%d conflicts=%d todayCleared=%d notCleared=%d unknown=%d' % (
        summary['fleetAccounts'], summary['fleetEnabled'], summary['masUsers'], summary['masMatched'],
        summary['masUnmatched'], summary['masAutoQueued'], summary['masNotAutoQueued'], summary['both'],
        summary['masOnly'], summary['fleetOnly'], summary['conflicts'],
        summary['todayCleared'], summary['todayNotCleared'], summary['todayUnknown']))
    if args.print:
        for r in rows:
            safe_print('  %-5s %-5s fleet=%-7s today=%-3s mas=%-6s today=%-3s emu=%-3s match=%s' % (
                r['key'] or '-', r['owner'], r['fleetDaily'] or '-', 'Y' if r['fleetToday'] else 'N',
                'ON' if r['masStatus'] else ('OFF' if r['masStatus'] is not None else '-'),
                'Y' if r['masToday'] else 'N', r['emu'] or '-', r['match']))


if __name__ == '__main__':
    main()
