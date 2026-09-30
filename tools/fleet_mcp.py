# -*- coding: utf-8 -*-
r"""maa-cli-fleet MCP —— 机队（日常/肉鸽）控制网关（stdio）。

给 Hermes / 任意 MCP 客户端直接调用机队运维脚本（ops/rogue_cli_ops.ps1）的能力面。

工具（11 个；读 2 + 写 9，**写动作默认 dry-run，需 apply=true 才真执行**）：

  只读
    fleet_status(machine="all")                体检：maa 进程 / 日志年龄 / 隧道 / 游戏
    fleet_log(tail=50, grep="")                读 ops 动作审计日志尾部

  写（默认 dry-run）
    fleet_daily(machine, apply=false)          只跑日常（自动先关游戏、停旧任务）
    fleet_rogue(machine, apply=false)          只刷肉鸽
    fleet_chain(machine, apply=false)          一次性：日常→关游戏→肉鸽
    fleet_cycle(machine, apply=false)          后台循环 worker：肉鸽↔日常 + 自愈
    fleet_cycle_stop(machine, apply=false)     停循环 worker
    fleet_watch(machine, apply=false)          后台看守 worker：只自愈
    fleet_watch_stop(machine, apply=false)     停看守 worker
    fleet_recover(machine, apply=false)        主机重启/掉线后恢复（启模拟器+双端 adb connect+隧道+发任务）
    fleet_fix(machine, step="fix", kind="", apply=false)
                                               手动阶梯修复：fix / fix1 / fix2 / fix3；kind=daily|rogue 可强制

机器名：l-1 / l-2 / l-4 / l-5 / l-7（或 all，支持的命令上）。

用法（自检面）：
    python tools/fleet_mcp.py --help        # 工具清单
    python tools/fleet_mcp.py --check       # 自检：依赖 + 脚本在位 + 只读 status 冒烟
    python tools/fleet_mcp.py               # 无参数 = stdio MCP server（Hermes 用 command+args 拉起）

审计：每次调用逐条落 ops/mcp_audit.jsonl（ts/tool/args/apply/rc）；真动作同时由运维脚本
      记入 ops/rogue_cli_ops.log（同一份审计源）。
"""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import json
import os
import pathlib
import shutil
import subprocess
import sys

PROJECT = pathlib.Path(__file__).resolve().parent.parent          # <root>\
OPS = PROJECT / "ops" / "rogue_cli_ops.ps1"
AUDIT = PROJECT / "ops" / "mcp_audit.jsonl"
OPS_LOG = PROJECT / "ops" / "rogue_cli_ops.log"

MACHINES = ("l-1", "l-2", "l-4", "l-5", "l-7")
SERVER_NAME = "maa-cli-fleet"

# 写命令 → 运维脚本子命令
WRITE_CMDS = {
    "fleet_daily": "daily",
    "fleet_rogue": "rogue",
    "fleet_chain": "chain",
    "fleet_cycle": "cycle",
    "fleet_cycle_stop": "cycle-stop",
    "fleet_watch": "watch",
    "fleet_watch_stop": "watch-stop",
    "fleet_recover": "recover",
}
ALL_OK = {"fleet_status", "fleet_recover", "fleet_cycle", "fleet_watch"}   # 支持 machine="all"

# --------------------------------------------------------------------------- #
# 基础                                                                         #
# --------------------------------------------------------------------------- #

def _log(msg: str) -> None:
    print(msg, file=sys.stderr, flush=True)


def _audit(rec: dict) -> None:
    rec = dict(rec)
    rec.setdefault("ts", dt.datetime.now().isoformat(timespec="seconds"))
    try:
        AUDIT.parent.mkdir(parents=True, exist_ok=True)
        with AUDIT.open("a", encoding="utf-8") as f:
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")
    except Exception:
        pass


def _fix_env() -> dict:
    """给子进程一个可用的 PATH（MCP 被极简 env 拉起时裸名 ssh/powershell 会失败）。"""
    env = dict(os.environ)
    extra = [
        r"C:\Windows\System32",
        r"C:\Windows",
        r"C:\Windows\System32\WindowsPowerShell\v1.0",
        r"C:\Program Files\Git\cmd",
        r"C:\Program Files\Git\usr\bin",
        str(PROJECT / "bin" / "adb"),
        str(PROJECT / "bin"),
    ]
    cur = env.get("PATH", "")
    env["PATH"] = os.pathsep.join([p for p in extra if p] + ([cur] if cur else []))
    env.setdefault("PYTHONUTF8", "1")
    env.setdefault("PYTHONUNBUFFERED", "1")
    return env


def _powershell() -> str:
    p = shutil.which("powershell")
    if p:
        return p
    return r"C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe"


def _run_ops(cmd: str, target: str = "all", kind: str = "", timeout: int = 180) -> dict:
    if not OPS.exists():
        return {"ok": False, "error": "ops script missing: %s" % OPS}
    argv = [_powershell(), "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", str(OPS), cmd]
    if target:
        argv.append(target)
    if kind:
        argv.append(kind)
    try:
        p = subprocess.run(argv, capture_output=True, text=True, encoding="utf-8",
                           errors="replace", timeout=timeout, env=_fix_env(),
                           stdin=subprocess.DEVNULL)
        out = (p.stdout or "").strip()
        err = (p.stderr or "").strip()
        return {"ok": p.returncode == 0, "rc": p.returncode, "cmdline": " ".join(argv[5:]),
                "output": out[-4000:], "stderr": err[-1000:] if err else ""}
    except subprocess.TimeoutExpired:
        return {"ok": False, "error": "timeout after %ss" % timeout, "cmdline": " ".join(argv[5:])}


def _check_machine(m: str, tool: str) -> str | None:
    if not m:
        return "machine 必填"
    if m not in MACHINES and not (m == "all" and tool in ALL_OK):
        return "未知机器 %r（可用：%s%s）" % (m, "/".join(MACHINES), "，或 all" if tool in ALL_OK else "")
    return None


# --------------------------------------------------------------------------- #
# 工具实现                                                                      #
# --------------------------------------------------------------------------- #

def t_status(a: dict) -> dict:
    m = str(a.get("machine", "all"))
    e = _check_machine(m, "fleet_status")
    if e:
        return {"ok": False, "error": e}
    return _run_ops("status", m, timeout=120)


def t_log(a: dict) -> dict:
    tail = int(a.get("tail", 50) or 50)
    kw = str(a.get("grep", "") or "")
    if not OPS_LOG.exists():
        return {"ok": True, "lines": [], "note": "日志尚未生成"}
    try:
        lines = OPS_LOG.read_text(encoding="utf-8", errors="replace").splitlines()
    except Exception as ex:
        return {"ok": False, "error": str(ex)}
    if kw:
        lines = [ln for ln in lines if kw in ln]
    return {"ok": True, "lines": lines[-max(1, min(tail, 500)):]}


def t_write(name: str, a: dict) -> dict:
    m = str(a.get("machine", ""))
    e = _check_machine(m, name)
    if e:
        return {"ok": False, "error": e}
    apply = bool(a.get("apply", False))
    sub = WRITE_CMDS[name]
    cmdline = "%s %s" % (sub, m)
    if not apply:
        return {"ok": True, "dry_run": True, "would_run": cmdline,
                "note": "写动作默认 dry-run；确认后加 apply=true 真执行"}
    res = _run_ops(sub, m, timeout=240 if sub == "recover" else 180)
    res["dry_run"] = False
    res["cmdline"] = cmdline
    return res


def t_fix(a: dict) -> dict:
    m = str(a.get("machine", ""))
    e = _check_machine(m, "fleet_fix")
    if e:
        return {"ok": False, "error": e}
    step = str(a.get("step", "fix") or "fix")
    if step not in ("fix", "fix1", "fix2", "fix3"):
        return {"ok": False, "error": "step 需为 fix/fix1/fix2/fix3"}
    kind = str(a.get("kind", "") or "")
    if kind and kind not in ("daily", "rogue"):
        return {"ok": False, "error": "kind 需为空或 daily/rogue"}
    apply = bool(a.get("apply", False))
    cmdline = " ".join(x for x in (step, m, kind) if x)
    if not apply:
        return {"ok": True, "dry_run": True, "would_run": cmdline,
                "note": "写动作默认 dry-run；确认后加 apply=true 真执行"}
    res = _run_ops(step, m, kind, timeout=400)
    res["dry_run"] = False
    res["cmdline"] = cmdline
    return res


def _call_tool(name: str, a: dict) -> dict:
    _audit({"tool": name, "args": {k: v for k, v in a.items() if k != "apply"}, "apply": bool(a.get("apply"))})
    if name == "fleet_status":
        return t_status(a)
    if name == "fleet_log":
        return t_log(a)
    if name == "fleet_fix":
        return t_fix(a)
    if name in WRITE_CMDS:
        return t_write(name, a)
    return {"ok": False, "error": "unknown tool %r" % name}


# --------------------------------------------------------------------------- #
# MCP 面                                                                        #
# --------------------------------------------------------------------------- #

def _schemas():
    import mcp.types as types

    def tool(name, desc, props, required=()):
        return types.Tool(name=name, description=desc,
                          inputSchema={"type": "object", "properties": props, "required": list(required)})

    m_prop = {"machine": {"type": "string", "description": "机器：%s（部分命令支持 all）" % "/".join(MACHINES)}}
    apply_prop = {"apply": {"type": "boolean", "description": "默认 false=dry-run；true 才真执行", "default": False}}
    return [
        tool("fleet_status", "体检机队：maa 进程 / 日志年龄 / 隧道 / 游戏（只读）", m_prop, ("machine",)),
        tool("fleet_log", "读机队动作审计日志尾部（只读）",
             {"tail": {"type": "integer", "default": 50}, "grep": {"type": "string", "description": "可选的过滤关键字"}}),
        tool("fleet_daily", "让某台只跑一轮日常（唤醒→刷理智→公招→基建→信用→奖励；会自动先关游戏并停掉该机旧任务）",
             {**m_prop, **apply_prop}, ("machine",)),
        tool("fleet_rogue", "让某台只刷肉鸽（自动先关游戏并停旧任务）", {**m_prop, **apply_prop}, ("machine",)),
        tool("fleet_chain", "一次性衔接：日常 → 关游戏 → 肉鸽", {**m_prop, **apply_prop}, ("machine",)),
        tool("fleet_cycle", "启动后台循环 worker（肉鸽↔日常：今日日常未完成先补；完成后按理智时钟；含自愈）",
             {**m_prop, **apply_prop}, ("machine",)),
        tool("fleet_cycle_stop", "停止循环 worker", {**m_prop, **apply_prop}, ("machine",)),
        tool("fleet_watch", "启动后台看守 worker（只自愈，不跑日常）", {**m_prop, **apply_prop}, ("machine",)),
        tool("fleet_watch_stop", "停止看守 worker", {**m_prop, **apply_prop}, ("machine",)),
        tool("fleet_recover", "主机重启/掉线恢复：启模拟器 → 查 adb 端口 → 双端 adb connect → 重建隧道 → 发任务",
             {**m_prop, **apply_prop}, ("machine",)),
        tool("fleet_fix", "手动阶梯修复：fix(①重发→②关游戏重开→③重启模拟器) 或 fix1/fix2/fix3；kind 可强制 daily/rogue",
             {**m_prop, "step": {"type": "string", "enum": ["fix", "fix1", "fix2", "fix3"], "default": "fix"},
              "kind": {"type": "string", "enum": ["", "daily", "rogue"], "default": ""}, **apply_prop}, ("machine",)),
    ]


async def _main() -> None:
    import mcp.types as types
    from mcp.server.lowlevel import Server
    from mcp.server.stdio import stdio_server

    schemas = _schemas()
    server = Server(SERVER_NAME)

    def _text(obj) -> list:
        return [types.TextContent(type="text", text=json.dumps(obj, ensure_ascii=False, indent=2))]

    @server.list_tools()
    async def _list() -> list:
        return schemas

    @server.call_tool()
    async def _call(tool_name: str, arguments: dict | None):
        try:
            return _text(_call_tool(tool_name, arguments or {}))
        except Exception as e:  # noqa: BLE001
            return _text({"ok": False, "error": "%s: %s" % (type(e).__name__, e)})

    async with stdio_server() as (read, write):
        await server.run(read, write, server.create_initialization_options())


# --------------------------------------------------------------------------- #
# CLI 面：--help / --check                                                      #
# --------------------------------------------------------------------------- #

def _check() -> int:
    problems, warnings = [], []
    try:
        import mcp  # noqa: F401
        print("[ok] mcp SDK 可用")
    except Exception as e:  # noqa: BLE001
        problems.append("mcp SDK 不可用：%s（pip install mcp）" % e)
    if not OPS.exists():
        problems.append("运维脚本缺失：%s" % OPS)
    else:
        print("[ok] 运维脚本在位：%s" % OPS)
    if not shutil.which("powershell") and not pathlib.Path(
            r"C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe").exists():
        problems.append("找不到 powershell")
    if not problems:
        r = _run_ops("status", "all", timeout=120)
        print("[%s] 只读冒烟 status rc=%s" % ("ok" if r.get("ok") else "warn", r.get("rc")))
        for ln in (r.get("output") or "").splitlines()[-6:]:
            print("   " + ln)
    for w in warnings:
        print("[warn] " + w)
    for p in problems:
        print("[ERR] " + p)
    return 2 if problems else 0


def _cli(argv=None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    ap = argparse.ArgumentParser(
        prog="fleet_mcp.py",
        description="maa-cli-fleet MCP server（stdio）。无参数启动 MCP server；本 CLI 面只做自检。",
        epilog="工具面（11 个）：fleet_status / fleet_log / fleet_daily / fleet_rogue / fleet_chain / "
               "fleet_cycle / fleet_cycle_stop / fleet_watch / fleet_watch_stop / fleet_recover / fleet_fix。"
               "写动作默认 dry-run（apply=true 才真执行）。")
    g = ap.add_mutually_exclusive_group()
    g.add_argument("--check", action="store_true", help="自检：依赖 + 脚本在位 + 只读 status 冒烟")
    ap.add_argument("--list", action="store_true", help="打印 11 个 MCP 工具名")
    a = ap.parse_args(argv)
    if a.check:
        return _check()
    if a.list:
        for t in _schemas():
            print("%-20s %s" % (t.name, t.description))
        return 0
    import asyncio
    asyncio.run(_main())
    return 0


if __name__ == "__main__":
    with contextlib.suppress(Exception):
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")  # type: ignore[attr-defined]
    sys.exit(_cli())
