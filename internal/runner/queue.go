package runner

import (
	"encoding/json"
	"fmt"
	"github.com/xiachk083-hub/maa-cli-fleet/internal/model"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 任务模型：一个账号的一次动作 = 一个原子任务。
//
//	key = "daily:a07" / "ann:a07"
//	state: queued → running → done / failed
//
// 调度器只做两件事：① 把"该做的任务"补进队列 ② 用 N 个并发位把队列跑掉。
// 没有"批次"概念；每个任务独立重试（最多 3 次）、独立记结果。
type Task struct {
	Key       string `json:"key"`
	AccountID string `json:"accountId"`
	Kind      string `json:"kind"` // daily | ann
	File      string `json:"file"` // 任务文件名：daily_a07 / ann_a07
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	Day       string `json:"day,omitempty"`  // daily 的口径（游戏日）
	Week      string `json:"week,omitempty"` // ann 的口径（本周周一）
	Enqueued  string `json:"enqueuedAt,omitempty"`
	Started   string `json:"startedAt,omitempty"`
	Ended     string `json:"endedAt,omitempty"`
	Note      string `json:"note,omitempty"`
	OutFile   string `json:"outFile,omitempty"`
	Priority  int    `json:"priority"`            // 越小越先跑
	NextDue   string `json:"nextDueAt,omitempty"` // 理智快满的时间点（自循环用）
	CostSec   int    `json:"costSec,omitempty"`   // 上次实际耗时（排班用）
	PlanAt    string `json:"planAt,omitempty"`    // 前瞻排班给它的执行时刻
	SanityCur int    `json:"sanityCur,omitempty"`
	SanityMax int    `json:"sanityMax,omitempty"`
}

const maxAttempts = 3

// queued 任务是否还该跑。
func (t *Task) pending() bool { return t.State == "queued" }

// sortQueue：待跑任务排序（优先级 → 入队时间）。
func sortQueue(ts []*Task) {
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].Priority != ts[j].Priority {
			return ts[i].Priority < ts[j].Priority
		}
		return ts[i].Enqueued < ts[j].Enqueued
	})
}

func nowStamp() string { return time.Now().Format("2006-01-02 15:04:05") }

// ---- 热改：queue.in（CLI 落请求 → runner 每轮合并）-----------------------------
//
// 解决"状态在 runner 内存里，外部改文件会被覆盖"的问题。
// 每行一个 JSON 请求：
//
//	{"op":"enqueue","kind":"daily","account":"a07"}   现在就跑（插队）
//	{"op":"cancel","task":"daily:a07"}                取消（今天不再跑）
//	{"op":"reset","task":"daily:a07"}                 失败重置（attempts=0 → 重新排队）
//	{"op":"now","task":"daily:a07"}                   把排班时间提到现在
//	{"op":"disable","account":"a07"} / {"op":"enable","account":"a07"}
type Op struct {
	Op      string `json:"op"`
	Kind    string `json:"kind,omitempty"`
	Account string `json:"account,omitempty"`
	Task    string `json:"task,omitempty"`
}

// ApplyOps 读取 queue.in 并合并到运行中的状态（由 Tick 调用）。
func (r *Runner) ApplyOps() int {
	path := filepath.Join(filepath.Dir(r.cfg.StateFile), "queue.in")
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var op Op
		if json.Unmarshal([]byte(line), &op) != nil {
			continue
		}
		if r.applyOp(op) {
			n++
		}
	}
	_ = os.Remove(path)
	if n > 0 {
		log.Printf("[queue.in] 应用了 %d 个热改请求", n)
	}
	return n
}

func (r *Runner) applyOp(op Op) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := op.Task
	if key == "" && op.Account != "" {
		k := op.Kind
		if k == "" {
			k = "daily"
		}
		key = k + ":" + op.Account
	}
	switch op.Op {
	case "enqueue", "now":
		t := r.state.Tasks[key]
		if t == nil {
			acc := r.findAccountLocked(strings.SplitN(key, ":", 2)[1])
			if acc == nil {
				return false
			}
			file := acc.Daily
			if strings.HasPrefix(key, "ann:") {
				file = acc.AnnTask
			}
			t = &Task{Key: key, AccountID: acc.ID, Kind: strings.SplitN(key, ":", 2)[0], File: file}
			r.state.Tasks[key] = t
		}
		t.State = "queued"
		t.Attempts = 0
		t.PlanAt = ""
		t.Priority = 5
		t.Day = gameDay()
		t.Enqueued = nowStamp()
		t.Note = "热改：现在就跑"
		return true
	case "cancel":
		if t := r.state.Tasks[key]; t != nil {
			t.State = "failed"
			t.Attempts = maxAttempts // 粘住：syncQueue 不会再把它捡回来
			t.Note = "热改：取消"
			return true
		}
	case "reset":
		if t := r.state.Tasks[key]; t != nil {
			t.State = "queued"
			t.Attempts = 0
			t.PlanAt = ""
			t.Day = gameDay()
			t.Note = "热改：重置重排"
			return true
		}
	case "disable", "enable":
		if a := r.findAccountLocked(op.Account); a != nil {
			a.Enabled = op.Op == "enable"
			return true
		}
	}
	return false
}

func (r *Runner) findAccountLocked(id string) *model.Account {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			return &r.accounts[i]
		}
	}
	return nil
}

// QueueOp 由 CLI 调用：把热改请求追加到 queue.in（不碰 state.json，避免被内存覆盖）。
func QueueOp(stateFile, op, arg string) error {
	path := filepath.Join(filepath.Dir(stateFile), "queue.in")
	var req Op
	req.Op = op
	switch op {
	case "enqueue", "now":
		parts := strings.SplitN(arg, ":", 2)
		if len(parts) == 2 {
			req.Kind, req.Account = parts[0], parts[1]
			req.Task = arg
		} else {
			req.Account = arg
			req.Kind = "daily"
		}
	case "cancel", "reset":
		req.Task = arg
	case "disable", "enable":
		req.Account = arg
	default:
		return fmt.Errorf("未知操作 %s", op)
	}
	b, _ := json.Marshal(req)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, 0x0A))
	return err
}
