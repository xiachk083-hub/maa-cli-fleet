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
	Slots           int      `json:"slots"`
	MumuManager     string   `json:"mumuManager"`
	AdbExe          string   `json:"adbExe"`
	MaaExe          string   `json:"maaExe"`
	ConfigDir       string   `json:"configDir"`
	DataRoot        string   `json:"stateRoot"`
	CacheDir        string   `json:"cacheDir"`
	LogDir          string   `json:"logDir"`
	LogFile         string   `json:"logFile"`
	AccountsFile    string   `json:"accountsFile"`
	StateFile       string   `json:"stateFile"`
	StopFile        string   `json:"stopFile"`
	CenterURL       string   `json:"centerUrl"`
	CenterTokenFile string   `json:"centerTokenFile"`
	NodeID          string   `json:"nodeId"`
	TaskTimeoutMin  int      `json:"taskTimeoutMin"`
	KeepEmus        []string `json:"keepEmus"`
	MaxConcurrent   int      `json:"maxConcurrent"`
	MinFreeRamMB    int      `json:"minFreeRamMB"`
	MaxCpuPct       int      `json:"maxCpuPct"`
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
	lanes    map[string]*rogueLane // 常驻肉鸽车道（accountID → lane）
	httpc    *http.Client
	token    string

	cpuIdle, cpuKern, cpuUser uint64
	cpuValid                  bool
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
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 12
	}
	if cfg.MinFreeRamMB <= 0 {
		cfg.MinFreeRamMB = 6000
	}
	if cfg.MaxCpuPct <= 0 {
		cfg.MaxCpuPct = 85
	}
	r := &Runner{cfg: cfg, state: &State{Tasks: map[string]*Task{}},
		running: map[string]bool{}, active: map[string]*Task{}, lanes: map[string]*rogueLane{},
		httpc: &http.Client{Timeout: 30 * time.Second}}

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
	// rogue 任务不在此列：它由常驻车道重建（每次启动都重新拉起）。
	for k, t := range r.state.Tasks {
		if t.Kind == "rogue" {
			delete(r.state.Tasks, k)
			continue
		}
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
		// 日常：自循环——理智快满（nextDueAt 到了）或还没跑过就跑；跑完按理智余量算下次时间。
		// Daily 为空 = 这台机不跑日常（如 l-5 只刷肉鸽）。
		dk := "daily:" + a.ID
		t := r.state.Tasks[dk]
		dueNow := false
		if a.Daily != "" {
			switch {
			case t == nil:
				dueNow = true
			case t.State == "failed" && t.Attempts < maxAttempts:
				dueNow = true
			case t.State == "done":
				// 前瞻排班优先：排班给的时间点到了才跑（这样就不会"到点一拥而上"）
				if t.PlanAt != "" {
					if ts, err := time.Parse("2006-01-02 15:04:05", t.PlanAt); err == nil {
						dueNow = time.Now().After(ts)
					}
				} else if t.NextDue == "" {
					dueNow = true // 老记录没算过 → 补算
				} else if ts, err := time.Parse("2006-01-02 15:04:05", t.NextDue); err == nil && time.Now().After(ts) {
					dueNow = true
				}
			}
		}
		if dueNow && !r.working(dk) {
			if t == nil {
				t = &Task{Key: dk, AccountID: a.ID, Kind: "daily", File: a.Daily, Priority: 10}
				r.state.Tasks[dk] = t
			}
			if t.State != "queued" {
				t.State = "queued"
				t.Attempts = 0
				t.Day = day
				t.Enqueued = nowStamp()
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
		// 肉鸽（常驻型）：这条不排队，只保证台账存在；真正的循环在 rogue.go 的车道里
		if a.RogueTask != "" {
			rk := "rogue:" + a.ID
			if r.state.Tasks[rk] == nil {
				r.state.Tasks[rk] = &Task{Key: rk, AccountID: a.ID, Kind: "rogue", File: a.RogueTask,
					State: "running", Enqueued: nowStamp(), Started: nowStamp(), Note: "常驻"}
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
	rogueN := 0
	var pend []string
	r.mu.Lock()
	for _, t := range r.state.Tasks {
		switch t.State {
		case "queued":
			queued++
			pend = append(pend, t.Key)
		case "running":
			if t.Kind == "rogue" {
				rogueN++
			} else {
				running++
			}
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
	log.Printf("任务队列｜游戏日 %s｜账号 %d｜并发上限 %d（在跑 %d，肉鸽常驻 %d）｜资源 空闲 %dMB / CPU %.0f%%", day, total, r.cfg.MaxConcurrent, running, rogueN, FreeRAMMB(), r.CPUPercent())
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
	r.SweepOrphans()
	r.startResidents() // 常驻肉鸽车道（不受并发位限制）
	tick := 0
	for {
		r.Tick()
		r.beat() // 心跳文件：外部看门狗据此识别“进程活着但卡死”
		tick++
		if tick%5 == 0 {
			r.BuildPlan() // 前瞻排班：把后面的活儿按容量铺开
		}
		if tick%10 == 0 {
			r.SweepOrphans()
		}
		if once {
			return
		}
		if r.cfg.StopFile != "" {
			if _, err := os.Stat(r.cfg.StopFile); err == nil {
				log.Printf("收到停止信号，等在场任务收尾…")
				r.stopResidents()
				r.drain(10 * time.Minute)
				_ = os.Remove(r.cfg.StopFile)
				return
			}
		}
		time.Sleep(30 * time.Second)
	}
}

// beat：写心跳文件（外部看门狗用它判断"进程活着但卡死"：文件 mtime 太旧 = 主循环没在跑）。
func (r *Runner) beat() {
	p := filepath.Join(filepath.Dir(r.cfg.StateFile), "heartbeat.txt")
	_ = os.WriteFile(p, []byte(time.Now().Format("2006-01-02 15:04:05")+"\n"), 0o644)
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
	r.ApplyOps() // 先合并外部热改请求（queue.in）
	added := r.syncQueue()
	r.saveState()

	// 资源闸门：占用过高就停止入队（等在场任务收工、资源回落）
	freeMB := FreeRAMMB()
	cpuPct := r.CPUPercent()
	r.mu.Lock()
	running := len(r.active)
	r.mu.Unlock()
	if r.Critical(freeMB) {
		log.Printf("[危险] 空闲内存仅 %dMB —— 已停止入队（建议减并发或加内存）", freeMB)
	}
	if gated, why := r.Gated(freeMB, cpuPct); gated {
		r.mu.Lock()
		waiting := 0
		for _, t := range r.state.Tasks {
			if t.State == "queued" {
				waiting++
			}
		}
		r.mu.Unlock()
		if running > 0 || waiting > 0 {
			log.Printf("[tick] 资源闸门（%s）：空闲内存 %dMB / CPU %.0f%% → 暂停入队（在跑 %d，待跑 %d）", why, freeMB, cpuPct, running, waiting)
		}
		return
	}

	r.mu.Lock()
	free := r.cfg.MaxConcurrent - running
	var ready []*Task
	for _, t := range r.state.Tasks {
		// 只领 daily/ann；rogue 是常驻车道，绝不能被并发位当一次性任务发出去
		if t.State == "queued" && t.Kind != "rogue" && !r.running[t.AccountID] {
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

// DispatchNow：任务一结束就立即补位（不等下一轮 30 秒 tick）——"清完自动接下个"。
func (r *Runner) DispatchNow() {
	defer func() { _ = recover() }()
	r.Tick()
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
