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
	"sync/atomic"
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
	AnnDisabled     bool     `json:"annDisabled"`            // true = 这轮先不刷剿灭（不排 ann 任务；state 里旧的也不动）
	RosterFile      string   `json:"rosterFile,omitempty"`   // 班表文件（默认：state.json 同目录的 roster.json）
	PlanEarlyMin    *int     `json:"planEarlyMin,omitempty"` // 前瞻排班最多提前多少分钟（默认 120；班表模式设 0 = 班次点发车）
	PlanLateMin     *int     `json:"planLateMin,omitempty"`  // 最多延后多少分钟（默认 240）
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
	tickMu   sync.Mutex      // serialize queue input, resource sampling and Tick
	running  map[string]bool // accountID → 该账号有任务在跑（串行保护）
	active   map[string]*Task
	lanes    map[string]*rogueLane // 常驻肉鸽车道（accountID → lane）
	httpc    *http.Client
	token    string

	roster *Roster // 班表（nil = 没有/关着 → 旧行为：理智自循环）

	maaPids   map[string]int // accountID → 最近一次发车的 maa pid（体检上报用，见 health.go）
	reporting atomic.Bool    // 上报防重入（体检含 adb 等慢调用）
	maintOn   atomic.Bool    // 维护模式状态（用于切换时只打一条日志，不刷屏）

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
		maaPids: map[string]int{}, httpc: &http.Client{Timeout: 30 * time.Second}}

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
	// 班表（可选）：有 = 日常按班次网格跑；没有 = 旧行为（理智自循环）
	r.roster = LoadRoster(rosterPath(cfg))
	r.roster.logLine(rosterPath(cfg))
	r.maintOn.Store(r.MaintenanceOn())
	return r, nil
}

// maintenancePath：维护开关文件（存在 = 维护中）。放在 state.json 同目录，机端运维直接看得到。
func (r *Runner) maintenancePath() string {
	return filepath.Join(filepath.Dir(r.cfg.StateFile), "maintenance")
}

// MaintenanceOn：维护模式 —— 人在手工动模拟器（登号/换号/排障）时用。
// 开着的时候：① 不派新车（队列只进不出）② 不扫孤儿模拟器（谁都不许关）。
// 肉鸽常驻车道照常跑（它们有自己的机器，不跟人抢）。
func (r *Runner) MaintenanceOn() bool {
	_, err := os.Stat(r.maintenancePath())
	return err == nil
}

// maintenanceFlip：返回（现在是否维护中，是否刚刚切换过来）—— 只在切换那一次打日志。
func (r *Runner) maintenanceFlip() (bool, bool) {
	on := r.MaintenanceOn()
	if on != r.maintOn.Swap(on) {
		return on, true
	}
	return on, false
}

// SetMaintenance：机端开关（on/off/status）。改文件不动进程，下一次 tick（≤30 秒）生效。
func SetMaintenance(cfgPath, action string) error {
	b, err := model.ReadFileBOM(cfgPath)
	if err != nil {
		return fmt.Errorf("读配置 %s：%w", cfgPath, err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("解析配置：%w", err)
	}
	p := filepath.Join(filepath.Dir(cfg.StateFile), "maintenance")
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "on":
		body := fmt.Sprintf("%s 维护模式：暂停派车 + 不扫模拟器（删掉本文件即恢复）\n", nowStamp())
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Printf("维护模式：开（%s）—— 从现在起不派新车、不关任何模拟器；肉鸽车道照常\n", p)
	case "off":
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Printf("维护模式：关（%s 已删除）—— 下一轮 tick 恢复派车与清扫\n", p)
	case "status", "":
		if _, err := os.Stat(p); err == nil {
			fmt.Printf("维护模式：开（%s）\n", p)
		} else {
			fmt.Printf("维护模式：关（%s 不存在）\n", p)
		}
	default:
		return fmt.Errorf("未知动作 %q（用法：-maintenance on|off|status）", action)
	}
	return nil
}

// recoverResidual：残留回收（上次进程被杀/崩了 → running 一律回队列，否则那些账号当天被跳过；
// rogue 记录删掉，由常驻车道重建）。只该由**真正要跑的进程**调用：
// 放在 New() 里会让 `-status` 每次都把状态改掉，永远显示不了真实的"在跑"（2026-10-02 修）。
func (r *Runner) recoverResidual() {
	r.mu.Lock()
	recovered := 0
	for k, t := range r.state.Tasks {
		if t.Kind == "rogue" {
			delete(r.state.Tasks, k)
			continue
		}
		if t.State == "running" {
			t.State = "queued"
			t.Note = "重启回收"
			recovered++
		}
	}
	r.mu.Unlock()
	if recovered > 0 {
		log.Printf("启动回收：%d 个残留 running → queued", recovered)
		r.saveState()
	}
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
			case t.State == "failed" && (t.Attempts < maxAttempts || t.Day != day):
				// 次数没用完 → 继续重试；次数用完但**游戏日换了** → 新的一天重新排（否则一次失败就永远不再跑）
				dueNow = true
			case t.State == "done":
				// 班表：reseed=true 时把"离班次最近的网格点"写入 NextDue（立即对齐）；
				// 默认不动 —— 下一次跑完由 scheduleNextDaily 落到网格上（首日不早饭/不浪费理智）
				if ro := r.roster; ro != nil && ro.Reseed && t.Ended != "" {
					if nd, ok := parseStamp(t.NextDue); ok && !ro.OnGrid(a.ID, nd) {
						// “清掉旧排班”：直接取从现在起的**第一个**网格点（不管上次跑了多久），
						// 于是全队在一个 cycle 内铺成连续流水线。首轮可能比理智满点早，但不丢理智：
						// 回复是连续的，只是这一轮花的点少一些。
						if want, _, ok := ro.SnapDue(a.ID, time.Time{}, time.Now()); ok {
							t.NextDue = want.Format(stampLayout)
							t.PlanAt = ""
							t.Note = "班表对齐"
						}
					}
				}
				// 前瞻排班优先：排班给的时间点到了才跑（这样就不会"到点一拥而上"）
				if t.PlanAt != "" {
					if ts, ok := parseStamp(t.PlanAt); ok {
						dueNow = time.Now().After(ts)
					}
				} else if t.NextDue == "" {
					dueNow = true // 老记录没算过 → 补算
				} else if ts, ok := parseStamp(t.NextDue); ok && time.Now().After(ts) {
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
		// 剿灭（按周）；annDisabled=true 时整块跳过（"先不刷"）
		if a.AnnTask != "" && !r.cfg.AnnDisabled {
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

// LogRoster 打印班表视图（fleet runner -status 用）：每班几台、下一轮各自什么时候跑。
func (r *Runner) LogRoster() {
	ro := r.roster
	if ro == nil {
		log.Printf("[班表] 未启用（日常按理智自循环）")
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	type row struct {
		id, shift string
		due       time.Time
	}
	var rows []row
	for _, a := range r.accounts {
		if !a.Enabled || a.Daily == "" || !ro.Has(a.ID) {
			continue
		}
		var lastRun time.Time
		var due time.Time
		if t := r.state.Tasks["daily:"+a.ID]; t != nil {
			lastRun, _ = parseStamp(t.Ended)
			switch {
			case t.NextDue == "":
				// 没有历史 → 按班表算
			case ro.Reseed:
				// reseed = 启动就把 NextDue 拉到从 now 起的第一个网格点（与 syncQueue 一致）
				if d, _, ok := ro.SnapDue(a.ID, time.Time{}, now); ok {
					due = d
				}
			default:
				due, _ = parseStamp(t.NextDue) // 已排好的不动（下次跑完才对齐）
			}
		}
		if due.IsZero() {
			if d, _, ok := ro.SnapDue(a.ID, lastRun, now); ok {
				due = d
			}
		}
		rows = append(rows, row{a.ID, ro.shiftOf(a.ID), due})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].due.Equal(rows[j].due) {
			return rows[i].due.Before(rows[j].due)
		}
		return rows[i].id < rows[j].id
	})
	per := map[string]int{}
	for _, x := range rows {
		per[x.shift]++
	}
	var names []string
	for _, s := range ro.Shifts {
		names = append(names, fmt.Sprintf("%s %d 台", s.Name, per[s.Name]))
	}
	log.Printf("[班表] 周期 %dh / 最少间隔 %dh｜%s｜共 %d 台", ro.CycleHours, ro.MinGapHours, strings.Join(names, "；"), len(rows))
	log.Printf("[班表] 接下来：")
	n := len(rows)
	if n > 16 {
		n = 16
	}
	for _, x := range rows[:n] {
		log.Printf("[班表]   %s  %-4s %s", x.due.Format("01-02 15:04"), x.id, x.shift)
	}
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
	maint := "关"
	if r.MaintenanceOn() {
		maint = "开（不派车 / 不扫模拟器）"
	}
	log.Printf("任务队列｜游戏日 %s｜账号 %d｜并发上限 %d（在跑 %d，肉鸽常驻 %d）｜维护 %s｜资源 空闲 %dMB / CPU %.0f%%", day, total, r.cfg.MaxConcurrent, running, rogueN, maint, FreeRAMMB(), r.CPUPercent())
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
	r.recoverResidual()
	r.Register()
	if r.MaintenanceOn() {
		log.Printf("[维护] 维护模式开着（%s）：不派车、不扫模拟器；删掉该文件即恢复", r.maintenancePath())
	}
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
	r.tickMu.Lock()
	defer r.tickMu.Unlock()
	r.ApplyOps() // 先合并外部热改请求（queue.in）
	added := r.syncQueue()
	r.saveState()

	// 维护模式：人在手工动模拟器（登号/换号/排障）——只入库不派车，也不扫地。
	if on, flipped := r.maintenanceFlip(); on {
		if flipped {
			log.Printf("[维护] 模式开启：暂停派车 + 不扫模拟器（肉鸽车道不受影响），删除 %s 恢复", r.maintenancePath())
		}
		if added > 0 {
			log.Printf("[维护] 新入队 %d 个任务，先存着不跑", added)
		}
		return
	} else if flipped {
		log.Printf("[维护] 模式关闭：恢复派车与清扫")
	}

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

	ready := r.claimTasks(time.Now())
	if added > 0 || len(ready) > 0 {
		log.Printf("[tick] 新入队 %d｜已领 %d", added, len(ready))
	}
	for _, t := range ready {
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

// claimTasks selects and reserves work in one critical section.
func (r *Runner) claimTasks(now time.Time) []*Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ready, claimed []*Task
	for _, t := range r.state.Tasks {
		a := r.findAccountLocked(t.AccountID)
		if a == nil || !a.Enabled || t.State != "queued" || (t.Kind != "daily" && t.Kind != "ann") || r.running[t.AccountID] {
			continue
		}
		if t.Kind == "daily" && a.Daily == "" {
			continue
		}
		if t.Kind == "ann" && r.cfg.AnnDisabled {
			continue
		}
		if t.PlanAt != "" {
			at, ok := parseStamp(t.PlanAt)
			if !ok || now.Before(at) {
				continue
			}
		}
		ready = append(ready, t)
	}
	sortQueue(ready)
	for _, t := range ready {
		if len(r.active) >= r.cfg.MaxConcurrent {
			break
		}
		if r.running[t.AccountID] {
			continue
		}
		t.State, t.Started = "running", nowStamp()
		t.Attempts++
		r.active[t.Key], r.running[t.AccountID] = t, true
		claimed = append(claimed, t)
	}
	return claimed
}

// DispatchNow：任务一结束就立即补位（不等下一轮 30 秒 tick）——"清完自动接下个"。
func (r *Runner) DispatchNow() {
	defer func() { _ = recover() }()
	r.Tick()
}

// FindAccount 找账号（持锁：SetRogue / disable 会改 r.accounts，任务 goroutine 可能并发读）。
func (r *Runner) FindAccount(id string) (model.Account, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.accounts {
		if a.ID == id {
			return a, true
		}
	}
	return model.Account{}, false
}
