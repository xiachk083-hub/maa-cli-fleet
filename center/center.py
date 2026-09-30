# -*- coding: utf-8 -*-
r"""fleet center —— 机队后端（中心服务）。

三层一体的"② 后端"：注册表 / 状态聚合 / 指令路由 / 审计 / 对外 API。
零依赖（Python 标准库 http.server），单进程常驻。

组件关系（详见 docs/ARCHITECTURE.md）：
    机端(node) ──注册/心跳/上报/拉令/回执──► 本服务 ──REST──► 前端(暂缓)/CLI/Agent

启动：
    python center/center.py --port 8790 [--bind 0.0.0.0] [--token <TOKEN>]
    首次启动若无 token ⇒ 自动生成并写 center/state/token.txt（机端与前端用它鉴权）

API（除 /health 外都要 `X-Fleet-Token` 头）：
    POST /register      {node:{id,ver,machines:{名:{...}}}}         机端→后端（上线登记）
    POST /report        {node_id, kind:"heartbeat|state|event", data} 机端→后端（状态/事件）
    GET  /poll?node_id=  （长轮询 ≤25s）→ {commands:[{id,cmd,machine,apply,kind,ts}]} 机端→后端（拉令）
    POST /result        {node_id,id,ok,rc,output}                   机端→后端（回执）
    POST /command       {node_id|"all",cmd,machine?,apply?,kind?}   控制端→后端（下发指令）
    GET  /fleet         聚合视图（机台/节点/最近事件）                控制端→后端
    GET  /health        存活（无鉴权）

存储（center/state/）：
    registry.json   节点与机台台账（含最近心跳/状态）
    events.jsonl    事件流（上报/指令/回执，只追加）
    commands.json   未完成指令队列（落盘兜底）
"""
from __future__ import annotations

import argparse
import json
import os
import pathlib
import queue
import secrets
import threading
import time
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

ROOT = pathlib.Path(__file__).resolve().parent.parent          # 项目根
STATE = ROOT / "center" / "state"
STATE.mkdir(parents=True, exist_ok=True)
REGISTRY = STATE / "registry.json"
EVENTS = STATE / "events.jsonl"
COMMANDS = STATE / "commands.json"
TOKEN_FILE = STATE / "token.txt"

LOCK = threading.RLock()
CMD_Q: dict[str, queue.Queue] = {}          # node_id -> 待发指令
PENDING: dict[str, dict] = {}               # cmd_id  -> 指令（含回执）
NODES: dict[str, dict] = {}                 # node_id -> 台账
TOKEN = ""


def now() -> str:
    return datetime.now().strftime("%Y-%m-%d %H:%M:%S")


def _load() -> None:
    global NODES, PENDING
    if REGISTRY.exists():
        try:
            data = json.loads(REGISTRY.read_text(encoding="utf-8"))
            NODES = data.get("nodes") or {}
            PENDING = data.get("pending") or {}
        except Exception:
            pass


def _save() -> None:
    with LOCK:
        tmp = REGISTRY.with_suffix(".tmp")
        tmp.write_text(json.dumps({"nodes": NODES, "pending": PENDING}, ensure_ascii=False, indent=2),
                       encoding="utf-8")
        tmp.replace(REGISTRY)


def _event(rec: dict) -> None:
    rec = {"ts": now(), **rec}
    with LOCK:
        with EVENTS.open("a", encoding="utf-8") as f:
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")


def _q(node_id: str) -> queue.Queue:
    with LOCK:
        if node_id not in CMD_Q:
            CMD_Q[node_id] = queue.Queue()
        return CMD_Q[node_id]


def enqueue(node_id: str, cmd: dict) -> dict:
    cid = "c" + secrets.token_hex(4)
    item = {"id": cid, "ts": now(), **cmd}
    with LOCK:
        PENDING[cid] = item
    _q(node_id).put(item)
    _event({"kind": "command", "node_id": node_id, "cmd": item})
    return item


# --------------------------------------------------------------------------- #
# HTTP                                                                          #
# --------------------------------------------------------------------------- #

class Handler(BaseHTTPRequestHandler):
    server_version = "fleet-center/0.1"

    def log_message(self, fmt, *args):  # 静音（事件流已是审计源）
        pass

    # -- helpers -------------------------------------------------------------
    def _json(self, code: int, obj) -> None:
        body = json.dumps(obj, ensure_ascii=False, indent=2).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _auth(self) -> bool:
        if TOKEN and self.headers.get("X-Fleet-Token") == TOKEN:
            return True
        self._json(401, {"ok": False, "error": "bad_token"})
        return False

    def _body(self) -> dict:
        try:
            n = int(self.headers.get("Content-Length") or 0)
            return json.loads(self.rfile.read(n).decode("utf-8")) if n else {}
        except Exception:
            return {}

    # -- routes --------------------------------------------------------------
    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/health":
            return self._json(200, {"ok": True, "ts": now(), "nodes": len(NODES)})
        if not self._auth():
            return
        if u.path == "/fleet":
            with LOCK:
                return self._json(200, {"ok": True, "ts": now(), "nodes": NODES,
                                        "pending": len(PENDING),
                                        "events": tail_events(50)})
        if u.path == "/poll":
            node_id = (parse_qs(u.query).get("node_id") or [""])[0]
            if not node_id:
                return self._json(400, {"ok": False, "error": "node_id_required"})
            with LOCK:
                if node_id in NODES:
                    NODES[node_id]["last_poll"] = now()
            try:
                item = _q(node_id).get(timeout=25)
                return self._json(200, {"ok": True, "commands": [item]})
            except queue.Empty:
                return self._json(200, {"ok": True, "commands": []})
        return self._json(404, {"ok": False, "error": "not_found"})

    def do_POST(self):
        if not self._auth():
            return
        u = urlparse(self.path)
        b = self._body()
        if u.path == "/register":
            node = b.get("node") or {}
            nid = node.get("id")
            if not nid:
                return self._json(400, {"ok": False, "error": "node.id_required"})
            with LOCK:
                prev = NODES.get(nid) or {}
                NODES[nid] = {**prev, **node, "last_seen": now(), "online": True}
                _save()
            _event({"kind": "register", "node_id": nid, "ver": node.get("ver"),
                    "machines": list((node.get("machines") or {}).keys())})
            return self._json(200, {"ok": True, "ts": now()})
        if u.path == "/report":
            nid = b.get("node_id")
            kind = b.get("kind", "state")
            if not nid:
                return self._json(400, {"ok": False, "error": "node_id_required"})
            with LOCK:
                n = NODES.setdefault(nid, {"id": nid})
                n["last_seen"] = now()
                n["online"] = True
                if kind == "state":
                    n["state"] = b.get("data") or {}
                elif kind == "heartbeat":
                    n["heartbeat"] = {"ts": now(), "data": b.get("data") or {}}
                _save()
            _event({"kind": kind, "node_id": nid,
                    "data": b.get("data") if kind == "event" else None})
            return self._json(200, {"ok": True})
        if u.path == "/result":
            cid = b.get("id")
            with LOCK:
                if cid in PENDING:
                    PENDING[cid]["result"] = {"ok": b.get("ok"), "rc": b.get("rc"),
                                              "output": (b.get("output") or "")[:4000], "ts": now()}
                    PENDING[cid]["done"] = True
                _save()
            _event({"kind": "result", "node_id": b.get("node_id"), "cmd_id": cid,
                    "ok": b.get("ok"), "rc": b.get("rc")})
            return self._json(200, {"ok": True})
        if u.path == "/command":
            target = b.get("node_id") or "all"
            cmd = {"cmd": b.get("cmd"), "machine": b.get("machine"),
                   "apply": bool(b.get("apply")), "kind": b.get("kind") or ""}
            if not cmd["cmd"]:
                return self._json(400, {"ok": False, "error": "cmd_required"})
            if target == "all":
                with LOCK:
                    targets = list(NODES.keys())
            else:
                targets = [target]
            queued = [enqueue(t, cmd)["id"] for t in targets]
            return self._json(200, {"ok": True, "queued": queued, "targets": targets})
        return self._json(404, {"ok": False, "error": "not_found"})


def tail_events(n: int = 50) -> list:
    if not EVENTS.exists():
        return []
    try:
        lines = EVENTS.read_text(encoding="utf-8", errors="replace").splitlines()[-n:]
        return [json.loads(ln) for ln in lines if ln.strip()]
    except Exception:
        return []


def main() -> int:
    global TOKEN
    ap = argparse.ArgumentParser(prog="center.py", description="机队后端（中心服务，stdlib 单进程）")
    ap.add_argument("--bind", default="0.0.0.0")
    ap.add_argument("--port", type=int, default=8790)
    ap.add_argument("--token", default="")
    ap.add_argument("--gen-token", action="store_true", help="重新生成 token 并退出")
    a = ap.parse_args()

    if TOKEN_FILE.exists() and not a.gen_token:
        TOKEN = TOKEN_FILE.read_text(encoding="utf-8").strip()
    if a.token:
        TOKEN = a.token
    if a.gen_token or not TOKEN:
        TOKEN = secrets.token_hex(16)
        TOKEN_FILE.write_text(TOKEN, encoding="utf-8")
        print("token 写入：%s" % TOKEN_FILE)
    if a.gen_token:
        print("新 token：%s" % TOKEN)
        return 0
    _load()
    srv = ThreadingHTTPServer((a.bind, a.port), Handler)
    print("[center] listening on %s:%d  (token file: %s)" % (a.bind, a.port, TOKEN_FILE))
    print("[center] 机端巡检: curl -H 'X-Fleet-Token: <token>' http://127.0.0.1:%d/fleet" % a.port)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        print("\n[center] bye")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
