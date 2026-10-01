// Package model —— 中心/机端/调度器共用的数据结构（JSON 形状与旧 PowerShell 端保持一致）。
package model

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
)

// ReadFileBOM 读文件并剥掉 UTF-8 BOM（PowerShell 的 Set-Content -Encoding UTF8 会带 BOM，
// 而 json.Unmarshal 不吃 BOM —— 这里是所有"读配置/状态"的唯一入口）。
func ReadFileBOM(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}), nil
}

// Result 指令回执。
type Result struct {
	OK     bool   `json:"ok"`
	RC     int    `json:"rc"`
	Output string `json:"output,omitempty"`
	TS     string `json:"ts"`
}

// Command 中心 → 机端的一条指令。
type Command struct {
	ID      string  `json:"id"`
	TS      string  `json:"ts"`
	Cmd     string  `json:"cmd"`
	Machine string  `json:"machine,omitempty"`
	Apply   bool    `json:"apply"`
	Kind    string  `json:"kind,omitempty"`
	Done    bool    `json:"done,omitempty"`
	Result  *Result `json:"result,omitempty"`
}

// Heartbeat 心跳载荷。
type Heartbeat struct {
	TS   string          `json:"ts"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Node 机端台账条目。
type Node struct {
	ID        string         `json:"id"`
	Ver       string         `json:"ver,omitempty"`
	Host      string         `json:"host,omitempty"`
	Machines  json.RawMessage `json:"machines,omitempty"`
	State     map[string]any `json:"state,omitempty"`
	Heartbeat *Heartbeat     `json:"heartbeat,omitempty"`
	LastSeen  string         `json:"last_seen,omitempty"`
	LastPoll  string         `json:"last_poll,omitempty"`
	Online    bool           `json:"online"`
}

// MachineState 一台机的状态（机端上报）。
type MachineState struct {
	Maa       string  `json:"maa"`
	LogAgeMin float64 `json:"logAgeMin"`
	Tunnel    string  `json:"tunnel"`
	Game      string  `json:"game"`
	Health    string  `json:"health"`
	SampledAt string  `json:"sampledAt"`
}

// Account 轮转器里的一个账号（runner/accounts.json）。
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Client   string `json:"client"`
	Stage    string `json:"stage"`
	Mode     string `json:"mode,omitempty"`
	Fallback string `json:"fallback"`
	Ann      string `json:"ann,omitempty"`
	Emu      string `json:"emu"`
	Port     string `json:"port,omitempty"`
	Daily    string `json:"daily"`
	AnnTask  string `json:"annTask,omitempty"`
	State    string `json:"state"`
	Enabled  bool   `json:"enabled"`

	// 肉鸽 = 常驻任务（与日常/剿灭同一套任务模型，只是策略不同）：
	// RogueTask 非空 → 这台机跑常驻肉鸽（跑完/挂了立刻重来，实例不关机）；
	// Resident 标记常驻机（不参与“跑完停机”的轮转）。
	RogueTask string `json:"rogueTask,omitempty"`
	Resident  bool   `json:"resident,omitempty"`
	StateDir  string `json:"stateDir,omitempty"` // MAA_STATE_DIR 覆盖（绝对路径；默认 stateRoot/state_<id>）
}

// AccountsFile 是 accounts.json 的顶层。
type AccountsFile struct {
	GeneratedAt string    `json:"generatedAt"`
	Slots       int       `json:"slots"`
	Accounts    []Account `json:"accounts"`
}

// NewID 生成一个短指令 id（形如 c1a2b3c4）。
func NewID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "c" + hex.EncodeToString(b)
}
