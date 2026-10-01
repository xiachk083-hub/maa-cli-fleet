// Package runner —— 槽位轮转调度器：一天内把全部账号的日常（+到期的剿灭）跑完。
//
// 自含：设备（MuMuManager/adb）、maa 发车、结果判定、状态记录、向 center 上报——都不依赖 PowerShell。
// 并发：每个槽一个 goroutine；Go 直接 Wait 子进程，不需要轮询 pid 文件。
package runner

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
	AccountsFile    string `json:"accountsFile"`
	StateFile       string `json:"stateFile"`
	CenterURL       string `json:"centerUrl"`
	CenterTokenFile string `json:"centerTokenFile"`
	NodeID          string `json:"nodeId"`
}

// State 是 runner/state.json。
type State struct {
	Accounts map[string]*AccState `json:"accounts"`
}

// AccState 单账号的跟踪状态。
type AccState struct {
	DoneDate string `json:"doneDate,omitempty"`
	LastEnd  string `json:"lastEnd,omitempty"`
	Fails    int    `json:"fails,omitempty"`
	LastNote string `json:"lastNote,omitempty"`
	LastOK   bool   `json:"lastOk,omitempty"`
}

const maxFailPerDay = 3

// Runner 是调度器实例。
type Runner struct {
	cfg      Config
	accounts []model.Account
	state    *State
	mu       sync.Mutex
	running  map[string]bool
	httpc    *http.Client
	token    string
}

// New 加载配置与账号表。
func New(cfgPath string) (*Runner, error) {
	b, err := model.ReadFileBOM(cfgPath)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	if cfg.Slots <= 0 {
		cfg.Slots = 8
	}
	r := &Runner{cfg: cfg, state: &State{Accounts: map[string]*AccState{}}, running: map[string]bool{}, httpc: &http.Client{Timeout: 30 * time.Second}}
	if b, err := model.ReadFileBOM(cfg.CenterTokenFile); err == nil {
		r.token = strings.TrimSpace(string(b))
	}
	if b, err := model.ReadFileBOM(cfg.AccountsFile); err == nil {
		var af model.AccountsFile
		if err := json.Unmarshal(b, &af); err == nil {
			r.accounts = af.Accounts
			if af.Slots > 0 && cfg.Slots <= 0 {
				r.cfg.Slots = af.Slots
			}
		}
	}
	if b, err := model.ReadFileBOM(cfg.StateFile); err == nil {
		_ = json.Unmarshal(b, r.state)
		if r.state.Accounts == nil {
			r.state.Accounts = map[string]*AccState{}
		}
	}
	return r, nil
}

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

// weekKey 返回本周（游戏日口径）周一的日期。
func weekKey() string {
	t := time.Now().Add(-4 * time.Hour)
	wd := (int(t.Weekday()) + 6) % 7 // 周一=0
	return t.AddDate(0, 0, -wd).Format("2006-01-02")
}

// Status 打印一眼状态。
func (r *Runner) Status() {
	day := gameDay()
	done, total := 0, 0
	r.mu.Lock()
	for _, a := range r.accounts {
		if !a.Enabled {
			continue
		}
		total++
		if s := r.state.Accounts[a.ID]; s != nil && s.DoneDate == day {
			done++
		}
	}
	running := len(r.running)
	r.mu.Unlock()
	log.Printf("游戏日 %s | 已完成 %d/%d | 在跑 %d/%d 槽", day, done, total, running, r.cfg.Slots)
}

// Run 常驻调度（或 Once 单轮）。
func (r *Runner) Run(once bool) {
	if !once {
		log.Printf("调度器启动：槽位 %d，账号 %d 个", r.cfg.Slots, len(r.accounts))
	}
	for {
		r.tick()
		if once {
			return
		}
		time.Sleep(60 * time.Second)
	}
}

// tick：把当天未完成的账号按“上次完成时间最旧优先”填进空槽。
func (r *Runner) tick() {
	day := gameDay()
	type cand struct {
		acc     model.Account
		lastEnd string
	}
	var due []cand
	r.mu.Lock()
	free := r.cfg.Slots - len(r.running)
	for _, a := range r.accounts {
		if !a.Enabled || r.running[a.ID] {
			continue
		}
		s := r.state.Accounts[a.ID]
		if s == nil {
			s = &AccState{}
			r.state.Accounts[a.ID] = s
		}
		if s.DoneDate == day {
			continue
		}
		le := s.LastEnd
		if le == "" {
			le = "2000-01-01T00:00:00"
		}
		due = append(due, cand{a, le})
	}
	r.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].lastEnd < due[j].lastEnd })
	log.Printf("[tick] 游戏日 %s | 空槽 %d | 待跑 %d", day, free, len(due))
	for i := 0; i < len(due) && i < free; i++ {
		a := due[i].acc
		r.mu.Lock()
		r.running[a.ID] = true
		r.mu.Unlock()
		go func(a model.Account) {
			defer func() {
				r.mu.Lock()
				delete(r.running, a.ID)
				r.mu.Unlock()
			}()
			r.slot(a)
		}(a)
	}
}

// slot：一个账号的一次“起机 → 日常 →（剿灭）→ 停机”。
func (r *Runner) slot(a model.Account) {
	day := gameDay()
	start := time.Now()
	logf := func(format string, args ...any) {
		log.Printf("["+a.ID+"] "+format, args...)
	}
	logf("开始：%s client=%s stage=%s emu=%s", a.Name, a.Client, a.Stage, a.Emu)

	port, err := DeviceUp(r.cfg, a.Emu)
	if err != nil {
		logf("设备未就绪：%v", err)
		r.finish(a, day, false, err.Error(), start)
		return
	}
	logf("设备就绪 127.0.0.1:%s", port)

	pkg := GamePackage(r.cfg, a.Client, port)
	state := map[string]any{
		"maa": "\u65e0", "logAgeMin": 9999, "tunnel": "\u901a",
		"game": GameRunning(r.cfg, port, pkg), "health": "!!\u9700\u5904\u7406",
		"sampledAt": time.Now().Format("01-02 15:04"),
	}

	// 日常
	ok, out, err := r.runTask(a, port, a.Daily)
	note := ""
	if err != nil {
		note = err.Error()
	}
	logf("日常结束 ok=%v note=%s out=%s", ok, note, filepath.Base(out))

	// 剿灭（本周没做就做）
	if ok && a.AnnTask != "" {
		wk := weekKey()
		markFile := filepath.Join(filepath.Dir(r.cfg.StateFile), "annweek_"+a.ID+".txt")
		if b, err := os.ReadFile(markFile); err != nil || strings.TrimSpace(string(b)) != wk {
			annOK, _, _ := r.runTask(a, port, a.AnnTask)
			logf("剿灭结束 ok=%v（周 %s）", annOK, wk)
			if annOK {
				_ = os.WriteFile(markFile, []byte(wk), 0o644)
			} else if note == "" {
				note = "剿灭失败"
			}
		} else {
			logf("剿灭本周已完成（%s），跳过", wk)
		}
	}

	if ok {
		state["health"] = "OK"
		state["maa"] = "\u5df2\u5b8c\u6210"
	}
	DeviceDown(r.cfg, a.Emu)
	logf("已停机")
	r.finish(a, day, ok, note, start)
	r.report(a.ID, state)
}

// runTask：发车 maa 并等它结束；返回 (无 Error, outFile, err)。
func (r *Runner) runTask(a model.Account, port, task string) (bool, string, error) {
	outFile := filepath.Join(r.cfg.LogDir, fmt.Sprintf("%s_%s.out", task, time.Now().Format("0102_150405")))
	f, err := os.Create(outFile)
	if err != nil {
		return false, outFile, err
	}
	defer f.Close()

	args := []string{"--batch", "run", task, "-a", "127.0.0.1:" + port}
	if a.Client != "" {
		args = append(args, "-p", a.Client)
	}
	cmd := exec.Command(r.cfg.MaaExe, args...)
	cmd.Dir = filepath.Dir(r.cfg.MaaExe)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Env = append(os.Environ(),
		"MAA_CONFIG_DIR="+r.cfg.ConfigDir,
		"MAA_DATA_DIR="+r.cfg.DataRoot,
		"MAA_CACHE_DIR="+r.cfg.CacheDir,
		"MAA_STATE_DIR="+filepath.Join(r.cfg.DataRoot, "state_"+a.ID),
	)
	if err := cmd.Start(); err != nil {
		return false, outFile, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(75 * time.Minute):
		_ = exec.Command("taskkill", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
		return false, outFile, fmt.Errorf("任务超时 75min")
	}
	// 结果判定：看 .out 里的 "… Error"
	b, _ := os.ReadFile(outFile)
	hasErr := regexp.MustCompile(`\]\s+Error`).Match(b)
	if hasErr {
		return false, outFile, nil
	}
	return true, outFile, nil
}

// finish：更新状态。
func (r *Runner) finish(a model.Account, day string, ok bool, note string, start time.Time) {
	r.mu.Lock()
	s := r.state.Accounts[a.ID]
	if s == nil {
		s = &AccState{}
		r.state.Accounts[a.ID] = s
	}
	s.LastEnd = time.Now().Format(time.RFC3339)
	s.LastOK = ok
	s.LastNote = note
	if ok {
		s.DoneDate = day
		s.Fails = 0
	} else {
		s.Fails++
		if s.Fails >= maxFailPerDay {
			s.DoneDate = day // 当日放弃
		}
	}
	fails := s.Fails
	elapsed := time.Since(start).Round(time.Second)
	r.mu.Unlock()
	r.saveState()
	log.Printf("[%s] 完成 ok=%v 耗时=%s 失败累计=%d %s", a.ID, ok, elapsed, fails, note)
}

// ---- 向 center 上报 ---------------------------------------------------------

func (r *Runner) report(id string, state map[string]any) {
	if r.cfg.CenterURL == "" || r.token == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"node_id": r.cfg.NodeID, "kind": "event",
		"data": map[string]any{"acc": id, "state": state}})
	req, _ := http.NewRequest("POST", strings.TrimRight(r.cfg.CenterURL, "/")+"/report", bytes.NewReader(body))
	req.Header.Set("X-Fleet-Token", r.token)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := r.httpc.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// ReportSummary 把当前全队状态推给 center（节点视角）。
func (r *Runner) ReportSummary() {
	if r.cfg.CenterURL == "" || r.token == "" {
		return
	}
	day := gameDay()
	machines := map[string]any{}
	r.mu.Lock()
	for _, a := range r.accounts {
		if !a.Enabled {
			continue
		}
		s := r.state.Accounts[a.ID]
		st := map[string]any{"health": "!!\u9700\u5904\u7406", "maa": "\u5f85\u8dd1"}
		if s != nil && s.DoneDate == day {
			st["health"] = "OK"
			st["maa"] = "\u4eca\u65e5\u5df2\u5b8c\u6210"
		}
		if r.running[a.ID] {
			st["maa"] = "\u8dd1\u7740"
		}
		if s != nil {
			st["fails"] = s.Fails
			st["lastNote"] = s.LastNote
		}
		machines[a.ID] = st
	}
	r.mu.Unlock()
	body, _ := json.Marshal(map[string]any{"node_id": r.cfg.NodeID, "kind": "state",
		"data": map[string]any{"machines": machines, "statusText": "runner"}})
	req, _ := http.NewRequest("POST", strings.TrimRight(r.cfg.CenterURL, "/")+"/report", bytes.NewReader(body))
	req.Header.Set("X-Fleet-Token", r.token)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := r.httpc.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
