// Package runner —— 任务队列调度器：一个账号的一次动作 = 一个原子任务；最多 N 个并发。
//
// 不是"分批"：队列里始终只有"该做/没做完"的任务，调度器用 8 个并发位把它们跑掉。
// 每个任务独立重试（≤3 次）、独立记录；同一账号的任务串行（不会两台 maa 抢一个模拟器）。
//
// 自含：设备（MuMuManager/adb）、maa 发车、结果判定、状态、向 center 上报——不依赖 PowerShell。
package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xiachk083-hub/maa-cli-fleet/internal/model"
)

// Config 是 runner/runner.json。
type Config struct {
	Slots           int    `json:"slots"`
	MumuManager     string `json:"mumuManager"`
	AdbExe          string `json:"adbExe"`
	MaaExe          string `json:"maaExe"`
	ConfigDir       string `json:"configDir"`
	DataRoot        string `json:"stateRoot"`
	CacheDir        string `json:"cacheDir"`
	LogDir          string `json:"logDir"`
	LogFile         string `json:"logFile"`
	AccountsFile    string `json:"accountsFile"`
	StateFile       string `json:"stateFile"`
	StopFile        string `json:"stopFile"`
	CenterURL       string `json:"centerUrl"`
	CenterTokenFile string `json:"centerTokenFile"`
	NodeID          string `json:"nodeId"`
	TaskTimeoutMin  int    `json:"taskTimeoutMin"`
}

// State 是 runner/state.json：任务台账。
type State struct {
	Tasks map[string]*Task `json:"tasks"`
}

// Runner 是调度器实例。
type Runner struct {
	cfg      Config
	accounts []model.Account
	state    *State
	mu       sync.Mutex
	running  map[string]bool // accountID → 该账号有任务在跑（串行保护）
	active   map[string]*Task
	httpc    *http.Client
	token    string
}

// New 加载配置/账号/状态（失败要吵：今天吃过"静默加载失败"的亏）。
func New(cfgPath string) (*Runner, error) {
	b, err := model.ReadFileBOM(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("读配置 %s：%w", cfgPath, err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置（注意 BOM/转义）：%w", err)
	}
	if cfg.Slots <= 0 {
		cfg.Slots = 8
	}
	if cfg.TaskTimeoutMin <= 0 {
		cfg.TaskTimeoutMin = 75
	}
	r := &Runner{cfg: cfg, state: &State{Tasks: map[string]*Task{}},
		running: map[string]bool{}, active: map[string]*Task{}, httpc: &http.Client{Timeout: 30 * time.Second}}

	if cfg.LogFile != "" {
		_ = os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755)
		f, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			log.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}
	if b, err := model.ReadFileBOM(cfg.CenterTokenFile); err == nil {
		r.token = strings.TrimSpace(string(b))
	}
	b, err = model.ReadFileBOM(cfg.AccountsFile)
	if err != nil {
		return nil, fmt.Errorf("读账号表 %s：%w（先跑 fleet gen）", cfg.AccountsFile, err)
	}
	var af model.AccountsFile
	if err := json.Unmarshal(b, &af); err != nil {
		return nil, fmt.Errorf("解析账号表：%w", err)
	}
	if len(af.Accounts) == 0 {
		return nil, fmt.Errorf("账号表里没有账号：%s", cfg.AccountsFile)
	}
	r.accounts = af.Accounts
	if b, err := model.ReadFileBOM(cfg.StateFile); err == nil {
		_ = json.Unmarshal(b, &r.state)
		if r.state.Tasks == nil {
			r.state.Tasks = map[string]*Task{}
		}
	}
	// 上次进程被杀/崩了 → 残留的 running 一律回收进队列（否则那些账号当天被跳过）
	for _, t := range r.state.Tasks {
		if t.State == "running" {
			t.State = "queued"
			t.Note = "重启回收"
		}
	}
	return r, nil
}

// Slots 返回当前并发位。
func (r *Runner) Slots() int { return r.cfg.Slots }

func (r *Runner) saveState() {
	r.mu.Lock()
	b, _ := json.MarshalIndent(r.state, "", "  ")
	r.mu.Unlock()
	tmp := r.cfg.StateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, r.cfg.StateFile)
	}
}

func gameDay() string { return time.Now().Add(-4 * time.Hour).Format("2006-01-02") }

// weekKey 返回本周（游戏日口径）周一。
func weekKey() string {
	t := time.Now().Add(-4 * time.Hour)
	wd := (int(t.Weekday()) + 6) % 7
	return t.AddDate(0, 0, -wd).Format("2006-01-02")
}

// ---- 队列维护 ---------------------------------------------------------------

// syncQueue：把"该做但还没做"的任务补进队列（幂等）。
func (r *Runner) syncQueue() (added int) {
	day, week := gameDay(), weekKey()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.accounts {
		if !a.Enabled {
			continue
		}
		// 日常
		dk := "daily:" + a.ID
		if t := r.state.Tasks[dk]; t == nil || (t.Day != day && !r.working(dk)) || (t.State == "failed" && t.Attempts < maxAttempts) {
			if t == nil || t.Day != day {
				r.state.Tasks[dk] = &Task{Key: dk, AccountID: a.ID, Kind: "daily", File: a.Daily,
					State: "queued", Day: day, Enqueued: nowStamp(), Priority: 10}
				added++
			}
		}
		// 剿灭（按周）
		if a.AnnTask != "" {
			ak := "ann:" + a.ID
			if t := r.state.Tasks[ak]; t == nil || (t.Week != week && !r.working(ak)) || (t.State == "failed" && t.Attempts < maxAttempts && t.Week == week) {
				if t == nil || t.Week != week {
					r.state.Tasks[ak] = &Task{Key: ak, AccountID: a.ID, Kind: "ann", File: a.AnnTask,
						State: "queued", Week: week, Enqueued: nowStamp(), Priority: 20}
					added++
				}
			}
		}
	}
	return added
}

// working：任务是否正在跑（调用方持锁）。
func (r *Runner) working(key string) bool {
	_, ok := r.active[key]
	return ok
}

// Enqueue 手工塞一个任务（fleet runner -enqueue daily:a07）。
func (r *Runner) Enqueue(spec string) error {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("格式：<daily|ann>:<accountId>")
	}
	kind, id := parts[0], parts[1]
	var acc *model.Account
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			acc = &r.accounts[i]
		}
	}
	if acc == nil {
		return fmt.Errorf("没有这个账号：%s", id)
	}
	file := acc.Daily
	if kind == "ann" {
		file = acc.AnnTask
		if file == "" {
			return fmt.Errorf("%s 没有剿灭任务", id)
		}
	}
	key := kind + ":" + id
	r.mu.Lock()
	r.state.Tasks[key] = &Task{Key: key, AccountID: id, Kind: kind, File: file,
		State: "queued", Day: gameDay(), Week: weekKey(), Enqueued: nowStamp(),
		Priority: 5, Note: "手工入队"}
	r.mu.Unlock()
	r.saveState()
	log.Printf("[queue] 手工入队 %s", key)
	return nil
}

// Cancel 取消一个任务。
func (r *Runner) Cancel(key string) error {
	r.mu.Lock()
	t, ok := r.state.Tasks[key]
	if ok && t.State != "running" {
		t.State = "failed"
		t.Note = "手工取消"
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("没有这个任务：%s", key)
	}
	r.saveState()
	return nil
}

// Status 打印队列视图。
func (r *Runner) Status() {
	day := gameDay()
	queued, running, doneD, doneA, failed := 0, 0, 0, 0, 0
	var pend []string
	r.mu.Lock()
	for _, t := range r.state.Tasks {
		switch t.State {
		case "queued":
			queued++
			pend = append(pend, t.Key)
		case "running":
			running++
		case "done":
			if t.Kind == "daily" && t.Day == day {
				doneD++
			} else if t.Kind == "ann" && t.Week == weekKey() {
				doneA++
			}
		case "failed":
			failed++
		}
	}
	total := len(r.accounts)
	r.mu.Unlock()
	sort.Strings(pend)
	log.Printf("任务队列｜游戏日 %s｜账号 %d｜并发位 %d（在跑 %d）", day, total, r.cfg.Slots, running)
	log.Printf("  今日日常完成 %d/%d｜本周剿灭完成 %d｜队列待跑 %d｜失败 %d%s", doneD, total, doneA, queued, failed,
		func() string {
			if len(pend) > 0 {
				n := len(pend)
				if n > 8 {
					n = 8
				}
				return "｜下一个：" + strings.Join(pend[:n], ", ")
			}
			return ""
		}())
}

// ---- 调度 -------------------------------------------------------------------

// Run 常驻（once=true 只跑一轮 tick）。
func (r *Runner) Run(once bool) {
	log.Printf("调度器启动：并发位 %d，账号 %d 个（任务队列模型）", r.cfg.Slots, len(r.accounts))
	for {
		r.Tick()
		if once {
			return
		}
		if r.cfg.StopFile != "" {
			if _, err := os.Stat(r.cfg.StopFile); err == nil {
				log.Printf("收到停止信号，等在场任务收尾…")
				r.drain(10 * time.Minute)
				_ = os.Remove(r.cfg.StopFile)
				return
			}
		}
		time.Sleep(30 * time.Second)
	}
}

func (r *Runner) drain(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.active)
		r.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// Tick：补队列 → 填并发位。
func (r *Runner) Tick() {
	added := r.syncQueue()
	r.saveState()

	r.mu.Lock()
	free := r.cfg.Slots - len(r.active)
	var ready []*Task
	for _, t := range r.state.Tasks {
		if t.State == "queued" && !r.running[t.AccountID] {
			ready = append(ready, t)
		}
	}
	r.mu.Unlock()
	sortQueue(ready)
	if added > 0 || len(ready) > 0 {
		log.Printf("[tick] 新入队 %d｜待跑 %d｜空位 %d", added, len(ready), free)
	}
	for i := 0; i < len(ready) && i < free; i++ {
		t := ready[i]
		r.mu.Lock()
		t.State = "running"
		t.Started = nowStamp()
		t.Attempts++
		r.active[t.Key] = t
		r.running[t.AccountID] = true
		r.mu.Unlock()
		go func(t *Task) {
			defer func() {
				r.mu.Lock()
				delete(r.active, t.Key)
				delete(r.running, t.AccountID)
				r.mu.Unlock()
			}()
			r.runTask(t)
		}(t)
	}
}

// FindAccount 找账号。
func (r *Runner) FindAccount(id string) (model.Account, bool) {
	for _, a := range r.accounts {
		if a.ID == id {
			return a, true
		}
	}
	return model.Account{}, false
}
