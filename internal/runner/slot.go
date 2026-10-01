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

	port, err := DeviceUp(r.cfg, acc.Emu)
	if err != nil {
		r.finishTask(t, false, "设备未就绪："+err.Error(), "")
		log.Printf("[%s] 设备未就绪：%v", t.Key, err)
		r.report(t, acc, "", false, "设备未就绪")
		return
	}
	log.Printf("[%s] 设备就绪 127.0.0.1:%s", t.Key, port)

	ok2, out, err := r.execMaa(acc, port, t.File)
	note := ""
	if err != nil {
		note = err.Error()
	}
	// 主关卡刷不了（活动关关闭等）→ 改刷兜底关卡（剩余理智，默认 1-7）再跑一次
	if !ok2 && t.Kind == "daily" && acc.Fallback != "" {
		if fbFile := r.makeFallbackTask(acc); fbFile != "" {
			log.Printf("[%s] 主关卡失败 → 改刷兜底关卡 %s（%s）", t.Key, acc.Fallback, fbFile)
			ok3, out3, err3 := r.execMaa(acc, port, fbFile)
			if err3 == nil && ok3 {
				ok2, out, note = true, out3, "主关卡不可用，已改刷兜底 "+acc.Fallback
			} else if err3 != nil {
				note = err3.Error()
			} else {
				note = "兜底关卡 " + acc.Fallback + " 也失败"
			}
		}
	}
	r.finishTask(t, ok2, note, out)
	DeviceDown(r.cfg, acc.Emu)
	log.Printf("[%s] 完成 ok=%v 耗时=%s out=%s", t.Key, ok2, time.Since(start).Round(time.Second), filepath.Base(out))
	r.report(t, acc, port, ok2, note)
}

// execMaa：跑一个任务文件并等它结束；返回 (无 Error, outFile, err)。
func (r *Runner) execMaa(a model.Account, port, task string) (bool, string, error) {
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
	case <-time.After(time.Duration(r.cfg.TaskTimeoutMin) * time.Minute):
		_ = exec.Command("taskkill", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
		return false, outFile, fmt.Errorf("任务超时 %d 分钟", r.cfg.TaskTimeoutMin)
	}
	b, _ := os.ReadFile(outFile)
	if regexp.MustCompile(`\]\s+Error`).Match(b) {
		return false, outFile, nil
	}
	return true, outFile, nil
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
