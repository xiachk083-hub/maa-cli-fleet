package runner

import (
	"log"
	"sort"
	"time"
)

// 前瞻排班（预约制）：不是"到点发现挤了就停"（后手），而是排班时就把容量算进去。
//
// 每个账号两个已知量：
//   · 截止 = 理智满的时刻（Task.NextDue）
//   · 耗时 = 上次实际耗时（Task.CostSec；没测过用默认 18 分钟）
// 排班：往后看 Horizon 小时，把账号塞进"那一刻还有空位（< slots）"的最早时段：
//   · 最早可提前 EarlyWindow（提前刷）
//   · 最晚可延后 LateWindow（延后刷）
//   · 再晚就要跨天漏刷 → 插队（priority=1）并尽量早排
type planItem struct {
	TaskKey string    `json:"taskKey"`
	Account string    `json:"account"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Note    string    `json:"note,omitempty"`
}

const (
	horizonMin   = 24 * 60 // 往后看 24 小时（分钟）
	earlyWindowM = 120     // 最多提前 2 小时
	lateWindowM  = 240     // 最多延后 4 小时
	defaultCostS = 18 * 60 // 没测过耗时的默认值（秒）
)

// BuildPlan 生成本轮排班表（并对齐任务的下次执行时间与优先级）。
func (r *Runner) BuildPlan() []planItem {
	now := time.Now()
	r.mu.Lock()
	var items []planItem
	for _, t := range r.state.Tasks {
		if t.Kind != "daily" {
			continue
		}
		if t.State != "done" && t.State != "queued" && t.State != "failed" {
			continue
		}
		// 截止时间
		var deadline time.Time
		if t.NextDue != "" {
			if ts, err := time.Parse("2006-01-02 15:04:05", t.NextDue); err == nil {
				deadline = ts
			}
		}
		if deadline.IsZero() {
			deadline = now.Add(30 * time.Minute) // 没算过 → 尽快
		}
		cost := defaultCostS
		if t.CostSec > 0 {
			cost = t.CostSec
		}
		items = append(items, planItem{TaskKey: t.Key, Account: t.AccountID,
			Start: deadline, End: deadline.Add(time.Duration(cost) * time.Second)})
	}
	r.mu.Unlock()

	// 按"截止早晚"排序，逐个塞进timeline
	sort.Slice(items, func(i, j int) bool { return items[i].Start.Before(items[j].Start) })
	slots := r.cfg.MaxConcurrent
	if slots <= 0 {
		slots = 4
	}
	// 只数"已经排好的"区间（标准贪心，避免同刻挤爆）
	placedRanges := [][2]time.Time{}
	load := func(start, end time.Time) int {
		n := 0
		for _, rng := range placedRanges {
			if rng[0].Before(end) && rng[1].After(start) {
				n++
			}
		}
		return n
	}
	for i := range items {
		it := &items[i]
		cost := it.End.Sub(it.Start)
		earliest := it.Start.Add(-time.Duration(earlyWindowM) * time.Minute)
		latest := it.Start.Add(time.Duration(lateWindowM) * time.Minute)
		placed := false
		for t := earliest; !t.After(latest); t = t.Add(5 * time.Minute) {
			if load(t, t.Add(cost)) < slots {
				it.Start, it.End = t, t.Add(cost)
				it.Note = "提前/延后至空位"
				placedRanges = append(placedRanges, [2]time.Time{it.Start, it.End})
				placed = true
				break
			}
		}
		if !placed { // 窗口内全满 → 插队：往后顺延到最早空位（仍按容量）
			for t := latest; ; t = t.Add(5 * time.Minute) {
				if load(t, t.Add(cost)) < slots {
					it.Start, it.End = t, t.Add(cost)
					if t.After(it.Start.Add(time.Duration(lateWindowM)*time.Minute)) {
						it.Note = "顺延（含跨天）"
					}
					placedRanges = append(placedRanges, [2]time.Time{it.Start, it.End})
					break
				}
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Start.Before(items[j].Start) })

	r.mu.Lock()
	for _, it := range items { // 回写：优先级 + 计划时间
		if t := r.state.Tasks[it.TaskKey]; t != nil {
			t.PlanAt = it.Start.Format("2006-01-02 15:04:05")
			if it.Note == "全满 → 插队" {
				t.Priority = 1
			} else if t.Priority < 10 {
				t.Priority = 10
			}
		}
	}
	r.mu.Unlock()
	return items
}

// LogPlan 打印排班表（前 12 条 + 预测并发峰值）。
func (r *Runner) LogPlan() {
	items := r.BuildPlan()
	if len(items) == 0 {
		log.Printf("[plan] 当前没有待排的日常")
		return
	}
	peak, peakAt := 0, time.Time{}
	for i := range items {
		n := 0
		for _, it := range items {
			if !it.Start.After(items[i].Start) && it.End.After(items[i].Start) {
				n++
			}
		}
		if n > peak {
			peak, peakAt = n, items[i].Start
		}
	}
	log.Printf("[plan] 未来 %d 小时内 %d 个日常，预计并发峰值 %d（%s），并发上限 %d",
		horizonMin/60, len(items), peak, peakAt.Format("01-02 15:04"), r.cfg.MaxConcurrent)
	n := len(items)
	if n > 12 {
		n = 12
	}
	for _, it := range items[:n] {
		log.Printf("[plan]   %s  %s → %s（%s）", it.Start.Format("01-02 15:04"), it.Account,
			it.End.Format("15:04"), it.Note)
	}
}
