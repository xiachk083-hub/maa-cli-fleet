package runner

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/xiachk083-hub/maa-cli-fleet/internal/model"
)

// ---- 常驻肉鸽（resident）------------------------------------------------------
//
// 肉鸽和日常/剿灭是同一套任务模型里的三种策略：
//
//	daily / ann —— 排队型：跑一次就结束，跑完排下一次；
//	rogue       —— 常驻型：只要这台机在就一直刷，跑完/挂了/卡死 → 立刻重来，实例不关机。
//
// 与日常的互斥：日常到点（理智快满）时插队——先暂停肉鸽（杀 maa + 关游戏），
// 日常跑完再放开，肉鸽自动接上。这就是原来 PS cycle-worker 的行为，搬进 runner。
//
// 看护判据（不靠 PID 存活、不靠固定超时）：
//
//	· maa 进程退出            → 重来（判 out 文件记成功/失败）
//	· asst.log 长时间不更新   → 判卡死，杀 maa 重来
//	· 连续两轮连设备都起不来 → 硬重启模拟器（坏 VM 阶梯）
const rogueStaleMin = 5 // 日志多少分钟没更新算卡死

type rogueLane struct {
	acc model.Account

	mu       sync.Mutex
	paused   bool
	stop     bool
	run      *maaRun
	note     string
	restarts int
	lastAt   time.Time
}

func (l *rogueLane) isPaused() bool   { l.mu.Lock(); defer l.mu.Unlock(); return l.paused }
func (l *rogueLane) stopped() bool    { l.mu.Lock(); defer l.mu.Unlock(); return l.stop }
func (l *rogueLane) setPaused(v bool) { l.mu.Lock(); l.paused = v; l.mu.Unlock() }
func (l *rogueLane) setStop(v bool)   { l.mu.Lock(); l.stop = v; l.mu.Unlock() }
func (l *rogueLane) setRun(r *maaRun) { l.mu.Lock(); l.run = r; l.mu.Unlock() }
func (l *rogueLane) curRun() *maaRun  { l.mu.Lock(); defer l.mu.Unlock(); return l.run }

func (l *rogueLane) setNote(n string) { l.mu.Lock(); l.note = n; l.lastAt = time.Now(); l.mu.Unlock() }

// status 给上报用的一句话。
func (l *rogueLane) status() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.stop:
		return "肉鸽停止"
	case l.paused:
		return "肉鸽暂停（日常）"
	case l.run != nil:
		return "肉鸽跑着"
	default:
		return "肉鸽重启中"
	}
}

// noteText 当前备注（给上报用）。
func (l *rogueLane) noteText() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.note
}

// ---- 生命周期 -----------------------------------------------------------------

// startResidents 给每个常驻账号起一条肉鸽车道（不受并发位限制：常驻就是常驻）。
func (r *Runner) startResidents() {
	for i := range r.accounts {
		a := r.accounts[i]
		if a.RogueTask == "" {
			continue
		}
		l := &rogueLane{acc: a, note: "常驻"}
		r.mu.Lock()
		r.lanes[a.ID] = l
		r.mu.Unlock()
		r.upsertRogueTask(a, "running", "常驻")
		go r.residentLoop(l)
	}
}

func (r *Runner) stopResidents() {
	r.mu.Lock()
	lanes := make([]*rogueLane, 0, len(r.lanes))
	for _, l := range r.lanes {
		lanes = append(lanes, l)
	}
	r.mu.Unlock()
	for _, l := range lanes {
		l.setStop(true)
		if run := l.curRun(); run != nil {
			killMaa(run)
		}
	}
}

// upsertRogueTask 维护 rogue:<id> 的任务台账（可见性用；不参与排队）。
func (r *Runner) upsertRogueTask(a model.Account, state, note string) *Task {
	key := "rogue:" + a.ID
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.state.Tasks[key]
	if t == nil {
		t = &Task{Key: key, AccountID: a.ID, Kind: "rogue", File: a.RogueTask, Enqueued: nowStamp()}
		r.state.Tasks[key] = t
	}
	t.File = a.RogueTask
	t.State = state
	t.Note = note
	if t.Started == "" {
		t.Started = nowStamp()
	}
	return t
}

// residentLoop 一台机的肉鸽常驻循环。
func (r *Runner) residentLoop(l *rogueLane) {
	a := l.acc
	log.Printf("[rogue:%s] 常驻肉鸽启动（任务 %s，实例 %s）", a.ID, a.RogueTask, a.Emu)
	fails := 0
	for {
		if l.stopped() {
			break
		}
		if l.isPaused() {
			time.Sleep(3 * time.Second)
			continue
		}

		// 设备：起机 → 等端口 → adb → boot（常驻机永不关机）
		port, err := DeviceUp(r.cfg, a.Emu)
		if err != nil {
			fails++
			l.setNote("设备未就绪：" + err.Error())
			r.upsertRogueTask(a, "running", "设备未就绪（第 "+strconv.Itoa(fails)+" 次）")
			log.Printf("[rogue:%s] 设备未就绪：%v（连续 %d 次）", a.ID, err, fails)
			if fails >= 2 {
				DeviceHardReset(r.cfg, a.Emu)
				fails = 0
			}
			sleepInterruptible(l, 60*time.Second)
			continue
		}
		fails = 0

		// 关游戏再发车：肉鸽被日常打断过/上次被杀，留着的画面会让 MAA 从脏状态起步
		forceStopGame(r.cfg, port, GamePackage(r.cfg, a.Client, port))

		run, err := r.startMaa(a, port, a.RogueTask)
		if err != nil {
			l.setNote("发车失败：" + err.Error())
			log.Printf("[rogue:%s] 发车失败：%v", a.ID, err)
			sleepInterruptible(l, 60*time.Second)
			continue
		}
		l.setRun(run)
		l.setNote("肉鸽跑着")
		r.upsertRogueTask(a, "running", "肉鸽跑着")
		log.Printf("[rogue:%s] 已发车 %s（pid=%d，日志 %s）", a.ID, a.RogueTask, run.cmd.Process.Pid, filepath.Base(run.out))

		// 看护：进程退出 / 被暂停 / 日志卡死
		done := make(chan error, 1)
		go func() { done <- run.cmd.Wait() }()
		lastMod := time.Now() // 从"现在"起算新鲜度（asst.log 是跨次追加的，不能用文件 mtime 起步）
		reason := ""
	watch:
		for {
			select {
			case <-done:
				reason = "任务退出"
				break watch
			default:
			}
			if l.stopped() {
				killMaa(run)
				reason = "收到停止"
				break watch
			}
			if l.isPaused() {
				killMaa(run)
				reason = "日常插队（暂停）"
				break watch
			}
			if mt := r.logModTime(a); mt.After(lastMod) {
				lastMod = mt
			} else if time.Since(lastMod) > rogueStaleMin*time.Minute {
				killMaa(run)
				reason = fmt.Sprintf("日志 %.0f 分钟没更新（卡死）", time.Since(lastMod).Minutes())
				break watch
			}
			time.Sleep(20 * time.Second)
		}
		<-done // 收尸
		l.setRun(nil)
		l.restarts++

		if l.stopped() {
			break
		}
		// 自然退出 → 判 out（成功/失败）；被杀 → 只记原因
		if reason == "任务退出" {
			ok, why := r.judgeOutFile(run.out)
			l.setNote("肉鸽" + map[bool]string{true: "完成", false: "失败"}[ok] + why)
			log.Printf("[rogue:%s] 肉鸽退出：ok=%v %s → 立即重来", a.ID, ok, why)
		} else {
			l.setNote(reason)
			log.Printf("[rogue:%s] 肉鸽中断：%s → 重来", a.ID, reason)
		}
		r.upsertRogueTask(a, "running", l.note)
		sleepInterruptible(l, 10*time.Second)
	}
	r.upsertRogueTask(a, "done", "已停止")
	log.Printf("[rogue:%s] 常驻肉鸽已停止", a.ID)
}

// ---- 与日常互斥 ---------------------------------------------------------------

// pauseResident 暂停某账号的肉鸽（日常要插队）：杀 maa + 等它真的停下（模拟器不动）。
func (r *Runner) pauseResident(accountID string) {
	r.mu.Lock()
	l := r.lanes[accountID]
	r.mu.Unlock()
	if l == nil {
		return
	}
	l.setPaused(true)
	if run := l.curRun(); run != nil {
		log.Printf("[rogue:%s] 日常插队 → 暂停肉鸽（杀 maa pid=%d）", accountID, run.cmd.Process.Pid)
		killMaa(run)
	}
	for i := 0; i < 60; i++ { // 最多等 30 秒
		if l.curRun() == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// resumeResident 日常跑完，放开肉鸽（residentLoop 会自己接上）。
func (r *Runner) resumeResident(accountID string) {
	r.mu.Lock()
	l := r.lanes[accountID]
	r.mu.Unlock()
	if l == nil {
		return
	}
	l.setPaused(false)
	log.Printf("[rogue:%s] 日常结束 → 放开肉鸽", accountID)
}

// ---- 小工具 -------------------------------------------------------------------

func sleepInterruptible(l *rogueLane, d time.Duration) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if l.stopped() {
			return
		}
		time.Sleep(time.Second)
	}
}

func killMaa(run *maaRun) {
	if run == nil || run.cmd == nil || run.cmd.Process == nil {
		return
	}
	_, _ = execOut(20*time.Second, "taskkill", "/PID", strconv.Itoa(run.cmd.Process.Pid), "/T", "/F")
}

func forceStopGame(cfg Config, port, pkg string) {
	if port == "" || pkg == "" {
		return
	}
	_, _ = execOut(20*time.Second, cfg.AdbExe, "-s", "127.0.0.1:"+port, "shell", "am", "force-stop", pkg)
}

// logModTime 该账号 MAA 状态日志的最后修改时间（取不到给零值）。
func (r *Runner) logModTime(a model.Account) time.Time {
	mt, err := statPath(filepath.Join(r.stateDirOf(a), "debug", "asst.log"))
	if err != nil {
		return time.Time{}
	}
	return mt
}

func statPath(p string) (time.Time, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// logAgeMin 日志年龄（分钟；取不到 = 很大）。
func (r *Runner) logAgeMin(a model.Account) float64 {
	mt := r.logModTime(a)
	if mt.IsZero() {
		return 9999
	}
	return time.Since(mt).Minutes()
}
