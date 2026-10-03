// fleet —— 机队一体化命令行（Go 主语言）
//
// 子命令：
//
//	fleet center   后端（中心服务：台账/指令/审计/API）
//	fleet node     机端（心跳/拉令/拉起 worker）        [阶段 1 晚些]
//	fleet runner   槽位轮转调度器（49 账号日常）        [阶段 1 晚些]
//	fleet gen      从 AUTO-MAS 配置生成账号表/任务文件   [阶段 1 晚些]
//	fleet version  版本
//
// 设计原则（见 docs/GO-MIGRATION.md）：Go 拿"大脑与骨架"，PowerShell 只做"手脚"
// （设备/模拟器现场操作），Go 通过 `ops.ps1 -Json` 调它，PS 不持有状态。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiachk083-hub/maa-cli-fleet/internal/center"
	"github.com/xiachk083-hub/maa-cli-fleet/internal/env"
	"github.com/xiachk083-hub/maa-cli-fleet/internal/gen"
	"github.com/xiachk083-hub/maa-cli-fleet/internal/node"
	"github.com/xiachk083-hub/maa-cli-fleet/internal/runner"
	"github.com/xiachk083-hub/maa-cli-fleet/internal/setup"
)

const version = "0.1.0"

func main() {
	root := projectRoot()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "center":
		addr := flagValue(args, "-addr", "0.0.0.0:8790")
		_ = addr
		stateDir := flagValue(args, "-state", filepath.Join(root, "center", "state"))
		token := flagValue(args, "-token", "")
		srv, err := center.New(stateDir, token)
		if err != nil {
			fmt.Fprintln(os.Stderr, "center 启动失败：", err)
			os.Exit(1)
		}
		if err := srv.ListenAndServe(addr); err != nil {
			fmt.Fprintln(os.Stderr, "center 退出：", err)
			os.Exit(1)
		}
	case "gen":
		autoMas := flagValue(args, "-automas", `E:\AUTO-MAS`)
		if err := gen.Run(autoMas, root, []string{"a25", "a28", "a31", "a34", "a52"}, hasFlag(args, "-dry")); err != nil {
			fmt.Fprintln(os.Stderr, "gen 失败：", err)
			os.Exit(1)
		}
	case "runner":
		cfg := flagValue(args, "-conf", filepath.Join(root, "runner", "runner.json"))
		r, err := runner.New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "runner 启动失败：", err)
			os.Exit(1)
		}
		if v := flagValue(args, "-enqueue", ""); v != "" {
			if err := runner.QueueOp(cfg, "enqueue", v); err != nil {
				fmt.Fprintln(os.Stderr, "入队失败：", err)
				os.Exit(1)
			}
			fmt.Println("已落入 queue.in（runner 30 秒内合并生效）")
			return
		}
		if v := flagValue(args, "-cancel", ""); v != "" {
			if err := runner.QueueOp(cfg, "cancel", v); err != nil {
				fmt.Fprintln(os.Stderr, "取消失败：", err)
				os.Exit(1)
			}
			return
		}
		if v := flagValue(args, "-reset", ""); v != "" {
			if err := runner.QueueOp(cfg, "reset", v); err != nil {
				fmt.Fprintln(os.Stderr, "重置失败：", err)
				os.Exit(1)
			}
			return
		}
		if v := flagValue(args, "-now", ""); v != "" {
			if err := runner.QueueOp(cfg, "now", v); err != nil {
				fmt.Fprintln(os.Stderr, "失败：", err)
				os.Exit(1)
			}
			return
		}
		if v := flagValue(args, "-disable", ""); v != "" {
			if err := runner.QueueOp(cfg, "disable", v); err != nil {
				fmt.Fprintln(os.Stderr, "失败：", err)
				os.Exit(1)
			}
			return
		}
		if v := flagValue(args, "-enable", ""); v != "" {
			if err := runner.QueueOp(cfg, "enable", v); err != nil {
				fmt.Fprintln(os.Stderr, "失败：", err)
				os.Exit(1)
			}
			return
		}
		if hasFlag(args, "-status") {
			r.Status()
			r.LogPlan()
			return
		}
		if hasFlag(args, "-once") {
			r.Run(true)
			r.ReportSummary()
			return
		}
		go func() {
			for {
				time.Sleep(60 * time.Second)
				r.ReportSummary()
			}
		}()
		r.Run(false)
	case "node":
		conf := flagValue(args, "-conf", filepath.Join(root, "node", "conf.json"))
		n, err := node.New(conf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "node 启动失败：", err)
			os.Exit(1)
		}
		n.Run()
	case "env":
		c := env.Cfg{
			MuMuManager: flagValue(args, "-mumu", "E:\\MuMu Player 12\\nx_main\\MuMuManager.exe"),
			Spec:        flagValue(args, "-spec", filepath.Join(root, "env", "desired.json")),
		}
		sub := ""
		if len(args) > 0 {
			sub = args[0]
		}
		emu := flagValue(args, "-emu", "")
		emus := emuList(root, flagValue(args, "-emus", ""), hasFlag(args, "-from-accounts"))
		var err error
		switch sub {
		case "show":
			err = env.Show(c, emu)
		case "set":
			_, err = env.SetOne(c, emu, flagValue(args, "-key", ""), flagValue(args, "-value", ""))
		case "restart":
			env.Restart(c, emu)
		case "check":
			_, err = env.Check(c, emus)
		case "converge":
			err = env.Converge(c, emus)
		default:
			fmt.Fprintln(os.Stderr, "用法：fleet env show|set|restart|check|converge [-emu N] [-emus 1,2,3] [-from-accounts] [-spec 文件]")
			os.Exit(2)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "env 失败：", err)
			os.Exit(1)
		}
	case "setup":
		o := setup.Options{
			Root:    flagValue(args, "-root", root),
			BinDir:  flagValue(args, "-bin", ""),
			DataDir: flagValue(args, "-data", ""),
			Channel: flagValue(args, "-channel", "stable"),
			Force:   hasFlag(args, "-force"),
		}
		if hasFlag(args, "-direct") {
			if err := setup.Direct(o.Root, o.DataDir); err != nil {
				fmt.Fprintln(os.Stderr, "setup -direct 失败：", err)
				os.Exit(1)
			}
			return
		}
		if err := setup.Run(o); err != nil {
			fmt.Fprintln(os.Stderr, "setup 失败：", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Printf("fleet %s (root=%s)\n", version, root)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令：%s\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `fleet —— 机队一体化命令行

用法：
  fleet center [-addr 0.0.0.0:8790] [-state <目录>] [-token <密钥>]
               另有 POST /mcp（Streamable HTTP，请求头带同一把 token）
  fleet setup  [-root <项目>] [-data <MAA_DATA_DIR>] [-force] [-channel stable]
  fleet runner [-conf <runner.json>] [-status] [-once] [-enqueue k:id] [-cancel key]
  fleet node   [-conf <conf.json>]
  fleet gen    [-automas <AUTO-MAS 目录>] [-dry]
  fleet version

根目录（自动定位）：exe 所在目录；可用 FLEET_ROOT 覆盖。
`)
}

// projectRoot：优先 FLEET_ROOT，否则用 exe 所在目录。
func projectRoot() string {
	if r := os.Getenv("FLEET_ROOT"); r != "" {
		return r
	}
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(exe)
}

// flagValue 取 -name value 形式的最小参数解析（避免为骨架引入 flag 面包屑）。
// emuList：取实例号清单（-emus 显式 / -from-accounts 从账号表取，自动去掉常驻的 5 台）。
func emuList(root, explicit string, fromAccounts bool) []string {
	keep := map[string]bool{"25": true, "28": true, "9": true, "34": true, "52": true}
	var out []string
	seen := map[string]bool{}
	if explicit != "" {
		for _, x := range strings.Split(explicit, ",") {
			x = strings.TrimSpace(x)
			if x != "" && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
		return out
	}
	if fromAccounts {
		b, err := os.ReadFile(filepath.Join(root, "runner", "accounts.json"))
		if err == nil {
			b = []byte(strings.TrimPrefix(string(b), "\uFEFF"))
			var af struct {
				Accounts []struct {
					Emu string `json:"emu"`
				} `json:"accounts"`
			}
			if json.Unmarshal(b, &af) == nil {
				for _, a := range af.Accounts {
					if a.Emu != "" && !keep[a.Emu] && !seen[a.Emu] {
						seen[a.Emu] = true
						out = append(out, a.Emu)
					}
				}
			}
		}
	}
	return out
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func flagValue(args []string, name, def string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if len(a) > len(name)+1 && a[:len(name)+1] == name+"=" {
			return a[len(name)+1:]
		}
	}
	return def
}
