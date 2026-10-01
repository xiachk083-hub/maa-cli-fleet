// Package node —— 机端（Go 版）：注册/心跳/状态上报/拉令执行/拉起 worker。
//
// 与 PowerShell 机端配置兼容（同一份 node/conf.json），可热替换。
package node

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/xiachk083-hub/maa-cli-fleet/internal/model"
	"time"
)

// Conf 是 node/conf.json（与 PS 版一致）。
type Conf struct {
	NodeID     string            `json:"nodeId"`
	CenterURL  string            `json:"centerUrl"`
	Token      string            `json:"token"`
	OpsScript  string            `json:"opsScript"`
	LogFile    string            `json:"logFile"`
	Heartbeat  int               `json:"heartbeatSec"`
	StateSec   int               `json:"stateSec"`
	Workers    map[string]string `json:"workers"`
	EmitStatus bool              `json:"emitStatus"`
}

const version = "0.2.0-go"

// Node 是机端实例。
type Node struct {
	conf   Conf
	httpc  *http.Client
	offset int64
	regAt  time.Time
}

// New 读取配置。
func New(confPath string) (*Node, error) {
	b, err := model.ReadFileBOM(confPath)
	if err != nil {
		return nil, err
	}
	var c Conf
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Heartbeat <= 0 {
		c.Heartbeat = 30
	}
	if c.EmitStatus == false {
		c.EmitStatus = true
	}
	if c.NodeID == "" || c.CenterURL == "" {
		return nil, fmt.Errorf("conf 缺 nodeId/centerUrl")
	}
	return &Node{conf: c, httpc: &http.Client{Timeout: 40 * time.Second}}, nil
}

// Run 常驻循环。
func (n *Node) Run() {
	log.Printf("[node] 启动：%s → %s", n.conf.NodeID, n.conf.CenterURL)
	n.ensureWorkers()
	for {
		n.step()
		time.Sleep(time.Duration(n.conf.Heartbeat) * time.Second)
	}
}

func (n *Node) step() {
	if time.Since(n.regAt) > 10*time.Minute {
		if n.post("/register", map[string]any{"node": map[string]any{
			"id": n.conf.NodeID, "ver": version, "host": hostname(),
		}}) == nil {
			n.regAt = time.Now()
		}
	}
	if n.conf.EmitStatus {
		st := n.collectState()
		_ = n.post("/report", map[string]any{"node_id": n.conf.NodeID, "kind": "state", "data": st})
	}
	_ = n.emitNewEvents()
	if cmd := n.poll(); cmd != nil {
		out := n.exec(cmd.Cmd, cmd.Machine, cmd.Kind, cmd.Apply)
		_ = n.post("/result", map[string]any{
			"node_id": n.conf.NodeID, "id": cmd.ID, "ok": out["ok"], "rc": out["rc"], "output": out["output"],
		})
	}
}

// ---- 状态采集：跑 ops status 并解析（与 PS 版同一条正则） ----------------------

var reStatus = regexp.MustCompile(`\[(\S+ \S+)\]\s+(l-\d+|a\d+)\s+\|\s+maa=(\S+)\s+\|\s+日志年龄=([\d.]+)分\s+\|\s+隧道=(\S+)\s+\|\s+游戏=(\S+)\s+\|\s+(\S+)`)

func (n *Node) collectState() map[string]any {
	out := n.runOps("status")
	machines := map[string]any{}
	for _, m := range reStatus.FindAllStringSubmatch(out, -1) {
		machines[m[2]] = map[string]any{
			"maa": m[3], "logAgeMin": m[4], "tunnel": m[5], "game": m[6], "health": m[7], "sampledAt": m[1],
		}
	}
	return map[string]any{"machines": machines, "statusText": strings.TrimSpace(out)}
}

func (n *Node) emitNewEvents() error {
	if n.conf.LogFile == "" {
		return nil
	}
	fi, err := os.Stat(n.conf.LogFile)
	if err != nil {
		return nil
	}
	if fi.Size() < n.offset {
		n.offset = 0
	}
	if fi.Size() == n.offset {
		return nil
	}
	f, err := os.Open(n.conf.LogFile)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(n.offset, io.SeekStart); err != nil {
		return err
	}
	b, _ := io.ReadAll(f)
	n.offset = fi.Size()
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	for len(lines) > 100 {
		lines = lines[1:]
	}
	return n.post("/report", map[string]any{"node_id": n.conf.NodeID, "kind": "event", "data": map[string]any{"lines": lines}})
}

// ---- 指令执行 ---------------------------------------------------------------

var validCmds = map[string]bool{
	"status": true, "daily": true, "rogue": true, "chain": true, "cycle": true, "cycle-stop": true,
	"watch": true, "watch-stop": true, "recover": true, "fix": true, "fix1": true, "fix2": true, "fix3": true,
	"run": true,
}

type cmd struct {
	ID      string `json:"id"`
	Cmd     string `json:"cmd"`
	Machine string `json:"machine"`
	Kind    string `json:"kind"`
	Apply   bool   `json:"apply"`
}

func (n *Node) exec(name, machine, kind string, apply bool) map[string]any {
	if !validCmds[name] {
		return map[string]any{"ok": false, "rc": 1, "output": "unknown_cmd:" + name}
	}
	writeCmds := map[string]bool{"daily": true, "rogue": true, "chain": true, "cycle": true, "cycle-stop": true,
		"watch": true, "watch-stop": true, "recover": true, "fix": true, "fix1": true, "fix2": true, "fix3": true, "run": true}
	plan := strings.TrimSpace(name + " " + machine + " " + kind)
	if writeCmds[name] && !apply {
		return map[string]any{"ok": true, "rc": 0, "output": "dry-run: " + plan + "（写指令需 apply=true）"}
	}
	log.Printf("[node] 执行 %s", plan)
	out := n.runOps(name, machine, kind)
	rc := 0
	if strings.Contains(out, "Error") && strings.Contains(out, "错误") {
		rc = 1
	}
	return map[string]any{"ok": rc == 0, "rc": rc, "output": out}
}

func (n *Node) runOps(args ...string) string {
	base := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", n.conf.OpsScript}
	all := append(base, args...)
	cmd := exec.Command("powershell", all...)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// ---- worker 守夜 -------------------------------------------------------------

func (n *Node) ensureWorkers() {
	if len(n.conf.Workers) == 0 {
		return
	}
	opsDir := filepath.Dir(n.conf.OpsScript)
	for machine, kind := range n.conf.Workers {
		if kind != "cycle" && kind != "watch" {
			continue
		}
		pidFile := filepath.Join(opsDir, kind+"_"+machine+".pid")
		b, err := os.ReadFile(pidFile)
		if err == nil && pidAlive(strings.TrimSpace(string(b))) {
			continue
		}
		log.Printf("[node] worker 缺失 → 拉起 %s %s", kind, machine)
		_ = n.runOps(kind, machine)
	}
}

// pidAlive：用 tasklist 判断 pid 是否活着。
func pidAlive(pid string) bool {
	if pid == "" {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", "PID eq "+pid, "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), ".exe")
}

// ---- HTTP ------------------------------------------------------------------

func (n *Node) poll() *cmd {
	req, err := http.NewRequest("GET", strings.TrimRight(n.conf.CenterURL, "/")+"/poll?node_id="+n.conf.NodeID, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("X-Fleet-Token", n.conf.Token)
	resp, err := n.httpc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var body struct {
		OK       bool  `json:"ok"`
		Commands []cmd `json:"commands"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || len(body.Commands) == 0 {
		return nil
	}
	return &body.Commands[0]
}

func (n *Node) post(path string, payload map[string]any) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", strings.TrimRight(n.conf.CenterURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("X-Fleet-Token", n.conf.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}
