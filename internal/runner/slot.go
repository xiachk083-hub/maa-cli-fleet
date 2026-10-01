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
	"strconv"
	"strings"
	"time"

	"github.com/xiachk083-hub/maa-cli-fleet/internal/model"
)

// runTask：一个原子任务的完整生命周期（起设备 → 发车 → 判定 → 停机 → 记状态 → 上报）。
func (r *Runner) runTask(t *Task) {
	acc, ok := r.FindAccount(t.AccountID)
	if !ok {
		r.finishTask(t, false, "账号不存在", "")
		return
	}
	start := time.Now()
	log.Printf("[%s] 开始 %s（%s，第 %d 次）", t.Key, acc.Name, t.Kind, t.Attempts)

	// 常驻机（肉鸽）：日常要插队 —— 先暂停肉鸽（杀 maa，模拟器不动），跑完自动接回
	if acc.Resident {
		r.pauseResident(acc.ID)
		defer r.resumeResident(acc.ID)
	}

	// 阶梯：第 2 次尝试开始先硬重启模拟器（坏 VM 重试没用，直接换一台干净的）
	if t.Attempts >= 2 {
		DeviceHardReset(r.cfg, acc.Emu)
	}

	port, err := DeviceUp(r.cfg, acc.Emu)
	if err != nil {
		r.finishTask(t, false, "设备未就绪："+err.Error(), "")
		log.Printf("[%s] 设备未就绪：%v", t.Key, err)
		r.report(t, acc, "", false, "设备未就绪")
		return
	}
	log.Printf("[%s] 设备就绪 127.0.0.1:%s", t.Key, port)

	// 常驻机：从肉鸽被打断的现场里起来，先关游戏（否则 MAA 从脏画面起步）
	if acc.Resident {
		forceStopGame(r.cfg, port, GamePackage(r.cfg, acc.Client, port))
	}

	ok2, out, err := r.execMaa(acc, port, t.File)
	note := ""
	if err != nil {
		note = err.Error()
	}
	r.finishTask(t, ok2, note, out)
	if ok2 {
		r.mu.Lock()
		t.CostSec = int(time.Since(start).Seconds())
		r.mu.Unlock()
	}
	if ok2 && t.Kind == "daily" {
		r.scheduleNextDaily(t, acc)
		r.saveState()
	}
	// 常驻机：永不关机（肉鸽要接着跑）；其它机：同实例还有活儿就连锁，否则停机
	if acc.Resident {
		log.Printf("[%s] 常驻机 → 保持开机，肉鸽接回", t.Key)
	} else if r.sameInstanceHasWork(acc) {
		log.Printf("[%s] 同实例还有活儿 → 保持开机，接着排下一个", t.Key)
	} else {
		DeviceDown(r.cfg, acc.Emu)
	}
	log.Printf("[%s] 完成 ok=%v 耗时=%s out=%s", t.Key, ok2, time.Since(start).Round(time.Second), filepath.Base(out))
	go r.DispatchNow()
	r.report(t, acc, port, ok2, note)
}

// stateDirOf：该账号的 MAA_STATE_DIR（肉鸽机沿用老目录，避免丢缓存）。
func (r *Runner) stateDirOf(a model.Account) string {
	if a.StateDir != "" {
		return a.StateDir
	}
	return filepath.Join(r.cfg.DataRoot, "state_"+a.ID)
}

// maaRun：一次 maa 进程（日常/剿灭/肉鸽共用）。
type maaRun struct {
	cmd  *exec.Cmd
	out  string
	task string
}

func (m *maaRun) closeOut() {
	// 进程结束/被杀后关掉我们持有的句柄（子进程有自己的句柄，不影响）
}

// startMaa 发车一个任务（不等待）；env 与旧 execMaa 完全一致。
func (r *Runner) startMaa(a model.Account, port, task string) (*maaRun, error) {
	outFile := filepath.Join(r.cfg.LogDir, fmt.Sprintf("%s_%s.out", task, time.Now().Format("0102_150405")))
	f, err := os.Create(outFile)
	if err != nil {
		return nil, err
	}
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
		"MAA_STATE_DIR="+r.stateDirOf(a),
	)
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &maaRun{cmd: cmd, out: outFile, task: task}, nil
}

// judgeOutFile：真核对（G7）——必须有任务链 Completed 行、且没有任务链 Error 行；
// 只“秒退”（资源/内核崩、TOML 错）那种什么都没有的，一律判失败。
func (r *Runner) judgeOutFile(outFile string) (bool, string) {
	b, _ := os.ReadFile(outFile)
	completed, errored := 0, 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "] Error") {
			// 主关卡不可用（活动关关闭等）不算整轮失败：后面的“刷剩余理智”会兜住
			if strings.Contains(line, "[刷理智]") {
				continue
			}
			errored++
		} else if strings.Contains(line, "] Completed") {
			completed++
		}
	}
	hardFail := regexp.MustCompile(`ExceptionCode 0x|Failed to find task file|TOML parse error|unknown variant`).Match(b)
	if errored > 0 || hardFail || completed == 0 {
		return false, fmt.Sprintf("completed=%d errored=%d hardFail=%v", completed, errored, hardFail)
	}
	return true, fmt.Sprintf("completed=%d", completed)
}

// execMaa：跑一个任务文件并等它结束；返回 (无 Error, outFile, err)。
func (r *Runner) execMaa(a model.Account, port, task string) (bool, string, error) {
	run, err := r.startMaa(a, port, task)
	if err != nil {
		return false, "", err
	}
	done := make(chan error, 1)
	go func() { done <- run.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(time.Duration(r.cfg.TaskTimeoutMin) * time.Minute):
		killMaa(run)
		<-done
		return false, run.out, fmt.Errorf("任务超时 %d 分钟", r.cfg.TaskTimeoutMin)
	}
	ok, why := r.judgeOutFile(run.out)
	if !ok {
		log.Printf("[judge] %s: %s → 失败", filepath.Base(run.out), why)
		return false, run.out, nil
	}
	log.Printf("[judge] %s: %s → 成功", filepath.Base(run.out), why)
	return true, run.out, nil
}

// sameInstanceHasWork：同一实例里是否还有其他账号的未完成任务（用于“不关机、接着跑”的连锁）。
func (r *Runner) sameInstanceHasWork(a model.Account) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.accounts {
		o := &r.accounts[i]
		if o.Emu != a.Emu || o.ID == a.ID || !o.Enabled {
			continue
		}
		for _, k := range []string{"daily:" + o.ID, "ann:" + o.ID} {
			if t := r.state.Tasks[k]; t != nil && (t.State == "queued" || t.State == "running") {
				return true
			}
		}
	}
	return false
}

// makeFallbackTask：生成"兜底关卡"版任务文件（把 stage 换成 fallback，如 1-7）。
func (r *Runner) makeFallbackTask(a model.Account) string {
	src := filepath.Join(r.cfg.ConfigDir, "tasks", a.Daily+".toml")
	b, err := os.ReadFile(src)
	if err != nil {
		return ""
	}
	re := regexp.MustCompile(`(stage\s*=\s*)"[^"]*"`)
	body := re.ReplaceAllString(string(b), `${1}"`+a.Fallback+`"`)
	name := a.Daily + "_fb"
	dst := filepath.Join(r.cfg.ConfigDir, "tasks", name+".toml")
	if os.WriteFile(dst, []byte(body), 0o644) != nil {
		return ""
	}
	return name
}

// readSanity：从 MAA 状态日志读最后一条 current/max sanity。
func (r *Runner) readSanity(a model.Account) (cur, max int) {
	p := filepath.Join(r.stateDirOf(a), "debug", "asst.log")
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, 0
	}
	re := regexp.MustCompile(`"current_sanity":(\d+),"max_sanity":(\d+)`)
	ms := re.FindAllStringSubmatch(string(b), -1)
	if len(ms) == 0 {
		return 0, 0
	}
	last := ms[len(ms)-1]
	cur, _ = strconv.Atoi(last[1])
	max, _ = strconv.Atoi(last[2])
	return cur, max
}

// scheduleNextDaily：按"理智还要多久满"排下一次日常（自循环）。
// 每点理智 6 分钟；留 30 分钟余量提前跑；最短 30 分钟后再来。
func (r *Runner) scheduleNextDaily(t *Task, a model.Account) {
	cur, max := r.readSanity(a)
	if max <= 0 {
		// 读不到（任务没跑成）→ 2 小时后重试，别死循环
		t.NextDue = time.Now().Add(2 * time.Hour).Format("2006-01-02 15:04:05")
		return
	}
	left := max - cur
	if left < 0 {
		left = 0
	}
	mins := left*6 - 30
	if mins < 30 {
		mins = 30
	}
	t.SanityCur, t.SanityMax = cur, max
	t.NextDue = time.Now().Add(time.Duration(mins) * time.Minute).Format("2006-01-02 15:04:05")
	log.Printf("[%s] 理智 %d/%d → 下次日常 %s（%.1f 小时后，自循环）", t.Key, cur, max, t.NextDue, float64(mins)/60)
}

// finishTask：写任务状态（未超次数的失败会回到 queued，下一轮 Tick 会再捡）。
func (r *Runner) finishTask(t *Task, ok bool, note, outFile string) {
	r.mu.Lock()
	t.Ended = nowStamp()
	t.Note = note
	if outFile != "" {
		t.OutFile = outFile
	}
	switch {
	case ok:
		t.State = "done"
	case t.Attempts >= maxAttempts:
		t.State = "failed"
	default:
		t.State = "queued"
	}
	r.mu.Unlock()
	r.saveState()
}

// ---- 上报 center -------------------------------------------------------------

func (r *Runner) report(t *Task, acc model.Account, port string, ok bool, note string) {
	if r.cfg.CenterURL == "" || r.token == "" {
		return
	}
	health, maa := "!!需处理", "失败"
	if ok {
		health, maa = "OK", "已完成"
	}
	r.postJSON("/report", map[string]any{
		"node_id": r.cfg.NodeID, "kind": "report",
		"data": map[string]any{"task": t.Key, "account": acc.ID, "ok": ok, "note": note},
	})
	r.postJSON("/report", map[string]any{
		"node_id": r.cfg.NodeID, "kind": "event",
		"data": map[string]any{"acc": acc.ID, "task": t.Key, "ok": ok, "note": note,
			"out": filepath.Base(t.OutFile), "health": health, "maa": maa},
	})
}

// ReportSummary：把队列与全队状态推给 center（常驻时每 30 秒一次）。
func (r *Runner) ReportSummary() {
	if r.cfg.CenterURL == "" || r.token == "" {
		return
	}
	day, week := gameDay(), weekKey()
	machines := map[string]any{}
	r.mu.Lock()
	doneN := 0
	for _, a := range r.accounts {
		if !a.Enabled {
			continue
		}
		st := map[string]any{"health": "!!需处理", "maa": "待跑"}
		if t := r.state.Tasks["daily:"+a.ID]; t != nil {
			switch {
			case t.State == "running":
				st["maa"] = "日常跑着"
			case t.State == "done" && t.Day == day:
				st["maa"], st["health"] = "日常完成", "OK"
				doneN++
			case t.State == "failed" && t.Day == day:
				st["maa"] = "日常失败"
			}
			st["note"] = t.Note
		}
		// 常驻肉鸽机：肉鸽才是主状态；日常状态另存一个字段（两者本来就能同时存在）
		if a.RogueTask != "" {
			if l := r.lanes[a.ID]; l != nil {
				rs := l.status()
				if dv, ok := st["maa"].(string); ok && dv != "待跑" {
					st["daily"] = dv
				}
				st["maa"] = rs
				if rs == "肉鸽跑着" && r.logAgeMin(a) < 5 {
					st["health"] = "OK"
				}
				if n := l.noteText(); n != "" {
					st["note"] = n
				}
			} else {
				st["maa"] = "肉鸽未启动"
			}
		}
		if a.AnnTask != "" {
			if t := r.state.Tasks["ann:"+a.ID]; t != nil && t.Week == week {
				st["ann"] = t.State
			}
		}
		machines[a.ID] = st
	}
	queued, running := 0, 0
	for _, t := range r.state.Tasks {
		if t.State == "queued" {
			queued++
		}
		if t.State == "running" {
			running++
		}
	}
	r.mu.Unlock()
	r.postJSON("/report", map[string]any{
		"node_id": r.cfg.NodeID, "kind": "state",
		"data": map[string]any{"machines": machines, "statusText": "runner",
			"queue": map[string]any{"queued": queued, "running": running, "slots": r.cfg.Slots, "doneToday": doneN}},
	})
}

func (r *Runner) postJSON(path string, payload map[string]any) {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", strings.TrimRight(r.cfg.CenterURL, "/")+path, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("X-Fleet-Token", r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.httpc.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
}
