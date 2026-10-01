package runner

import (
	"sort"
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
	Priority  int    `json:"priority"` // 越小越先跑
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
