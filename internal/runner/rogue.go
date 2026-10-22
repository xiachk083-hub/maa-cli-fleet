package runner

import (
	"bytes"
	"fmt"
	"io"
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
//	· 同一 cur_task 连续空转  → 杀 maa 重来（日志还在写，mtime 看护看不见）
//	· 连续两轮连设备都起不来 → 硬重启模拟器（坏 VM 阶梯）
const rogueStaleMin = 5 // 日志多少分钟没更新算卡死
const rogueSpinMin = 8  // 同一 cur_task 连续多久算空转。2026-10-05 健康局最长 2.5 分钟（l-1 Begin / l-5 空任务）

type rogueLane struct {
	acc model.Account

	mu        sync.Mutex
	paused    bool
	stop      bool
	run       *maaRun
	note      string
	task      string // 当前任务名（热切换时更新；空 = 用 acc.RogueTask）
	switching bool   // 当前 maa 是被 setRogue 杀掉的：退出后不判 out、不睡 10 秒
	restarts  int
	lastAt    time.Time
	idle      chan struct{} // non-nil covers DeviceUp, force-stop, launch, Wait and cleanup
}

func (l *rogueLane) isPaused() bool   { l.mu.Lock(); defer l.mu.Unlock(); return l.paused }
func (l *rogueLane) stopped() bool    { l.mu.Lock(); defer l.mu.Unlock(); return l.stop }
func (l *rogueLane) setPaused(v bool) { l.mu.Lock(); l.paused = v; l.mu.Unlock() }
func (l *rogueLane) setStop(v bool)   { l.mu.Lock(); l.stop = v; l.mu.Unlock() }
func (l *rogueLane) setRun(r *maaRun) { l.mu.Lock(); l.run = r; l.mu.Unlock() }
func (l *rogueLane) curRun() *maaRun  { l.mu.Lock(); defer l.mu.Unlock(); return l.run }

func (l *rogueLane) setNote(n string) { l.mu.Lock(); l.note = n; l.lastAt = time.Now(); l.mu.Unlock() }

// beginCycle and pause use the same mutex: no launch can slip past pause.
func (l *rogueLane) beginCycle() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.paused || l.stop {
		return false
	}
	l.idle = make(chan struct{})
	return true
}
func (l *rogueLane) endCycle() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.idle != nil {
		close(l.idle)
		l.idle = nil
	}
}

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
		if !a.Enabled || a.RogueTask == "" {
			continue
		}
		l := &rogueLane{acc: a, task: a.RogueTask, note: "常驻"}
		r.mu.Lock()
		r.lanes[a.ID] = l
		r.mu.Unlock()
		r.upsertRogueTask(a, a.RogueTask, "running", "常驻")
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
func (r *Runner) upsertRogueTask(a model.Account, task, state, note string) *Task {
	key := "rogue:" + a.ID
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.state.Tasks[key]
	if t == nil {
		t = &Task{Key: key, AccountID: a.ID, Kind: "rogue", File: task, Enqueued: nowStamp()}
		r.state.Tasks[key] = t
	}
	t.File = task
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
	log.Printf("[rogue:%s] 常驻肉鸽启动（任务 %s，实例 %s）", a.ID, l.taskName(), a.Emu)
	fails := 0
	for {
		if l.stopped() {
			break
		}
		r.laneBeat(a) // paused lanes are alive during arbitrarily long daily runs
		if l.isPaused() {
			time.Sleep(3 * time.Second)
			continue
		}
		r.laneBeat(a)
		task := l.taskName()

		if !l.beginCycle() {
			continue
		}
		func() {
			defer l.endCycle()
			// 设备：起机 → 等端口 → adb → boot（常驻机永不关机）
			port, err := DeviceUp(r.cfg, a.Emu)
			if err != nil {
				fails++
				l.setNote("设备未就绪：" + err.Error())
				r.upsertRogueTask(a, task, "running", "设备未就绪（第 "+strconv.Itoa(fails)+" 次）")
				log.Printf("[rogue:%s] 设备未就绪：%v（连续 %d 次）", a.ID, err, fails)
				if fails >= 2 {
					DeviceHardReset(r.cfg, a.Emu)
					fails = 0
				}
				sleepInterruptible(l, 60*time.Second)
				return
			}
			fails = 0

			// 关游戏再发车：肉鸽被日常打断过/上次被杀，留着的画面会让 MAA 从脏状态起步。
			// 但暂停信号随时可能到 —— 到就不动手（别人正在跑日常，不能 force-stop 抢设备）
			if l.isPaused() {
				sleepInterruptible(l, 2*time.Second)
				return
			}
			forceStopGame(r.cfg, port, GamePackage(r.cfg, a.Client, port))
			if l.isPaused() {
				sleepInterruptible(l, 2*time.Second)
				return
			}

			task = l.taskName() // 设备准备期间可能被热切换
			run, err := r.startMaa(a, port, task)
			if err != nil {
				l.setNote("发车失败：" + err.Error())
				log.Printf("[rogue:%s] 发车失败：%v", a.ID, err)
				sleepInterruptible(l, 60*time.Second)
				return
			}
			// 抢跑：暂停信号在起机过程里到了 → 立刻把这把收回去，别和日常抢设备
			if l.isPaused() || l.stopped() {
				killMaa(run)
				_ = run.cmd.Wait() // reap a launch raced by pause before releasing ownership
				return
			}
			l.setRun(run)
			l.setNote("肉鸽跑着")
			r.upsertRogueTask(a, task, "running", "肉鸽跑着")
			log.Printf("[rogue:%s] 已发车 %s（pid=%d，日志 %s）", a.ID, task, run.cmd.Process.Pid, filepath.Base(run.out))

			// 起机过程中被热切换：刚发的旧任务立刻收回，走下面的“切换任务”分支。
			if l.taskName() != task {
				l.markSwitch()
				killMaa(run)
			}

			// 看护：进程退出 / 被暂停 / 日志卡死
			// 注意：Wait() 的返回值只能收一次！自己退出（case 收到）就不能再 `<-done`，
			// 否则永久阻塞（2026-10-02 14:03 l-4 肉鸽自然退出后车道卡死 6 小时，就是这个坑）。
			done := make(chan error, 1)
			go func() { done <- run.cmd.Wait() }()
			lastMod := time.Now() // 从“现在”起算新鲜度（asst.log 是跨次追加的，不能用文件 mtime 起步）
			lastBeat := time.Now()
			reason := ""
			exited := false
			var spinTask string
			var spinSince time.Time
			spinSeen := false
			for {
				select {
				case <-done:
					exited = true
					reason = "任务退出"
				default:
				}
				if exited {
					break
				}
				if l.stopped() {
					killMaa(run)
					reason = "收到停止"
					break
				}
				if l.isPaused() {
					killMaa(run)
					reason = "日常插队（暂停）"
					break
				}
				r.laneBeat(a)
				if mt := r.logModTime(a); mt.After(lastMod) {
					lastMod = mt
				} else if time.Since(lastMod) > rogueStaleMin*time.Minute {
					killMaa(run)
					reason = fmt.Sprintf("日志 %.0f 分钟没更新（卡死）", time.Since(lastMod).Minutes())
					break
				}
				if task, ok := lastCurTask(filepath.Join(r.stateDirOf(a), "debug", "asst.log")); ok {
					if !spinSeen || task != spinTask {
						spinTask = task
						spinSince = time.Now()
						spinSeen = true
					} else if time.Since(spinSince) >= rogueSpinMin*time.Minute {
						killMaa(run)
						name := task
						if name == "" {
							name = "(空)"
						}
						reason = fmt.Sprintf("同一任务 %s 已 %.0f 分钟（空转）", name, time.Since(spinSince).Minutes())
						break
					}
				}
				// 心跳（每 ~10 分钟一条）：车道没声音不代表在干活，留个心跳好排查
				if time.Since(lastBeat) > 10*time.Minute {
					log.Printf("[rogue:%s] 看护中：maa pid=%d，日志 %.1f 分钟前更新", a.ID, run.cmd.Process.Pid, r.logAgeMin(a))
					lastBeat = time.Now()
				}
				time.Sleep(20 * time.Second)
			}
			if !exited {
				<-done // 是我们杀的 → 等它真的收尸；自己退的上面已经收到，不能再收一次
			}
			l.setRun(nil)
			l.restarts++

			if l.stopped() {
				return
			}
			// 热切换：不判旧任务的 out、不睡 10 秒，立刻用新任务重发。
			if l.takeSwitch() {
				task = l.taskName()
				l.setNote("切换任务：" + task)
				log.Printf("[rogue:%s] 切换任务 → %s", a.ID, task)
				r.upsertRogueTask(a, task, "running", "切换任务")
				return
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
			r.upsertRogueTask(a, task, "running", l.noteText())
			sleepInterruptible(l, 10*time.Second)
		}()
	}
	r.upsertRogueTask(a, l.taskName(), "done", "已停止")
	log.Printf("[rogue:%s] 常驻肉鸽已停止", a.ID)
}

// ---- 与日常互斥 ---------------------------------------------------------------

// pauseResident 暂停某账号的肉鸽（日常要插队）：杀 maa + 等它真的停下（模拟器不动）。
func (r *Runner) pauseResident(accountID string) error {
	return r.pauseResidentWithin(accountID, 30*time.Second)
}

func (r *Runner) pauseResidentWithin(accountID string, timeout time.Duration) error {
	r.mu.Lock()
	l := r.lanes[accountID]
	r.mu.Unlock()
	if l == nil {
		return nil
	}
	l.mu.Lock()
	l.paused = true
	idle := l.idle
	l.mu.Unlock()
	// The cycle owns kill and Wait; acknowledgement includes their completion.
	if idle == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return nil
	case <-timer.C:
		return fmt.Errorf("resident %s did not quiesce within %s", accountID, timeout)
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
		if l.stopped() || l.isPaused() {
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

// laneBeat 单个车道的心跳文件：外部看门狗（boot_runner.ps1）据此判断"某个车道卡死"。
// 主循环心跳（heartbeat.txt）只能发现整进程卡死；车道是独立 goroutine，得单独留痕。
func (r *Runner) laneBeat(a model.Account) {
	p := filepath.Join(filepath.Dir(r.cfg.StateFile), "hb_rogue_"+a.ID+".txt")
	_ = os.WriteFile(p, []byte(time.Now().Format("2006-01-02 15:04:05")+"\n"), 0o644)
}

// lastCurTask 读 asst.log 尾巴，取最后一条 cur_task。日志还在刷、但任务名不变 = 空转。
// 找不到（文件不在 / 尾巴里没有这个字段）返回 false，调用方不要据此杀进程。
func lastCurTask(path string) (string, bool) {
	const tail = 256 << 10
	b, truncated, err := readTail(path, tail)
	if err != nil || len(b) == 0 {
		return "", false
	}
	// 只有 seek 进来的尾巴才可能切到半行。整文件读取时第一行是完整的，不能丢。
	if truncated {
		if i := bytes.IndexByte(b, '\n'); i >= 0 && i < len(b)-1 {
			b = b[i+1:]
		}
	}
	key := []byte(`cur_task":"`)
	i := bytes.LastIndex(b, key)
	if i < 0 {
		return "", false
	}
	rest := b[i+len(key):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return "", false
	}
	return string(rest[:j]), true
}

func readTail(path string, n int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	truncated := false
	if fi.Size() > n {
		truncated = true
		if _, err := f.Seek(fi.Size()-n, io.SeekStart); err != nil {
			return nil, false, err
		}
	}
	b, err := io.ReadAll(f)
	return b, truncated, err
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
