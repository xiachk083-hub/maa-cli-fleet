package runner

import (
	"log"
	"sort"
	"strings"
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
	// ---- 自优化：把"离上一批很近却单独开"的任务并进上一批（发车间隔），并反复几轮直到收敛 ----
	const gapMin = 60      // 与上一批间隔小于此文 = 不值得单独开，并入
	const maxCluster = 6   // 一批最多几台（cap 与"合适大小"取小）
	clusterCap := slots
	if clusterCap > maxCluster {
		clusterCap = maxCluster
	}
	for pass := 0; pass < 3; pass++ {
		sort.Slice(items, func(i, j int) bool { return items[i].Start.Before(items[j].Start) })
		moved := 0
		for i := range items {
			it := &items[i]
			cost := it.End.Sub(it.Start)
			// 找它前面最近的一批（起点不同但时间接近的组）
			var prev *planItem
			for j := 0; j < len(items); j++ {
				if j == i {
					continue
				}
				o := &items[j]
				if o.Start.Equal(it.Start) || o.Start.After(it.Start) {
					continue
				}
				gap := it.Start.Sub(o.Start)
				if gap > 0 && gap <= time.Duration(gapMin)*time.Minute {
					if prev == nil || o.Start.After(prev.Start) {
						prev = o
					}
				}
			}
			if prev == nil {
				continue
			}
			// 想并到 prev 那一批：检查那一批的并发是否放得下，以及不早于自己的最早可提前
			batchStart := prev.Start
			if batchStart.Before(it.Start.Add(-time.Duration(earlyWindowM) * time.Minute)) {
				continue // 提前太多，不划算
			}
			if load(batchStart, batchStart.Add(cost)) >= clusterCap {
				continue // 那一批已经够大
			}
			it.Start, it.End = batchStart, batchStart.Add(cost)
			it.Note = "并入上一批"
			placedRanges = append(placedRanges, [2]time.Time{it.Start, it.End})
			moved++
		}
		if moved == 0 {
			break
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
	// 按批次（同一开始时刻）聚合展示
	i := 0
	for i < len(items) {
		batchStart := items[i].Start
		var names []string
		for i < len(items) && items[i].Start.Equal(batchStart) {
			names = append(names, items[i].Account)
			i++
		}
		note := ""
		if len(items) > 0 && i-1 < len(items) {
			note = items[i-1].Note
		}
		log.Printf("[plan]   批次 %s：%d 台（%s）%s", batchStart.Format("01-02 15:04"), len(names),
			strings.Join(names, ","), note)
	}
}
