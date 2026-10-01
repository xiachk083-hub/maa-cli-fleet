// Package model —— 中心/机端/调度器共用的数据结构（JSON 形状与旧 PowerShell 端保持一致）。
package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

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
