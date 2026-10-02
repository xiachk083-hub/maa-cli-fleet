package runner

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ---- 模拟器 / adb / maa 的“手脚”（Go 直接调子进程，自含，不依赖 PS）----------

var rePort = regexp.MustCompile(`"adb_port":\s*(\d+)`)
var reStarted = regexp.MustCompile(`"is_process_started":\s*true`)
var reIndex = regexp.MustCompile(`"index":\s*"(\d+)"`)

// execOut 跑一条命令并拿输出（带超时）。
func execOut(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// MuMuInfo 查实例信息（JSON 文本）。
func MuMuInfo(mgr, idx string) string {
	out, _ := execOut(60*time.Second, mgr, "info", "-v", idx)
	return out
}

// mumuInst 是 info -v all 里单个实例的形状。
// 注意：JSON 字段是字母序，adb_port / is_process_started 都排在 index **前面**，
// 所以绝不能对整个输出按行解析（会把属性算到上一个实例头上 → SweepOrphans 关错模拟器，2026-10-02 修）。
// 也不能用 json.Unmarshal：实例名里有未转义字符，官方输出不是严格 JSON。
// 就按对象切块（"<idx>": { ... }），块内取字段。
type mumuInst struct {
	Index   string
	AdbPort string
	Started bool
}

var reObj = regexp.MustCompile(`(?s)"(\d+)":\s*\{(.*?)\}`)

// MuMuAll 解析 info -v all：index → 实例信息。
func MuMuAll(mgr string) map[string]mumuInst {
	out, _ := execOut(60*time.Second, mgr, "info", "-v", "all")
	m := map[string]mumuInst{}
	for _, mm := range reObj.FindAllStringSubmatch(out, -1) {
		key, body := mm[1], mm[2]
		inst := mumuInst{Index: key}
		if s := rePort.FindStringSubmatch(body); len(s) == 2 {
			inst.AdbPort = s[1]
		}
		inst.Started = reStarted.MatchString(body)
		m[key] = inst
	}
	return m
}

// EmuRunning 实例是否在跑。
func EmuRunning(mgr, idx string) bool {
	return reStarted.MatchString(MuMuInfo(mgr, idx))
}

// EmuPort 实例当前的 adb 端口（0 = 没拿到）。
func EmuPort(mgr, idx string) string {
	m := rePort.FindStringSubmatch(MuMuInfo(mgr, idx))
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// EmuLaunch 启动实例（脱离本进程：Windows 下用 cmd /c start 拉开，避免随父进程死）。
func EmuLaunch(mgr, idx string) {
	_, _ = execOut(30*time.Second, "cmd", "/c", "start", "", mgr, "control", "--vmindex", idx, "launch")
}

// EmuShutdown 关闭实例。
func EmuShutdown(mgr, idx string) {
	_, _ = execOut(60*time.Second, mgr, "control", "--vmindex", idx, "shutdown")
}

// AdbConnect 连接设备。
func AdbConnect(adb, port string) string {
	out, _ := execOut(20*time.Second, adb, "connect", "127.0.0.1:"+port)
	return strings.TrimSpace(out)
}

// AdbBoot 设备是否启动完成（"1" = 好）。
func AdbBoot(adb, port string) string {
	out, _ := execOut(20*time.Second, adb, "-s", "127.0.0.1:"+port, "shell", "getprop", "sys.boot_completed")
	return strings.TrimSpace(out)
}

// DeviceUp 起机 → 等端口 → adb connect → 等 boot；返回可用端口。
func DeviceUp(cfg Config, emu string) (string, error) {
	if !EmuRunning(cfg.MumuManager, emu) {
		EmuLaunch(cfg.MumuManager, emu)
		time.Sleep(3 * time.Second)
	}
	for i := 0; i < 90; i++ {
		time.Sleep(5 * time.Second)
		port := EmuPort(cfg.MumuManager, emu)
		if port == "" {
			continue
		}
		AdbConnect(cfg.AdbExe, port)
		if AdbBoot(cfg.AdbExe, port) == "1" {
			return port, nil
		}
	}
	return "", fmt.Errorf("模拟器未就绪（emu=%s）", emu)
}

// DeviceDown 关机释放内存。
func DeviceDown(cfg Config, emu string) {
	EmuShutdown(cfg.MumuManager, emu)
	time.Sleep(5 * time.Second)
}

// GamePackage 解析游戏包名：显式 > 客户端映射 > adb 探测。
func GamePackage(cfg Config, client, port string) string {
	byClient := map[string]string{
		"Official": "com.hypergryph.arknights",
		"Bilibili": "com.hypergryph.arknights.bilibili",
		"YoStarEN": "com.YoStarEN.Arknights",
		"YoStarJP": "com.YoStarJP.Arknights",
		"YoStarKR": "com.YoStarKR.Arknights",
		"txwy":     "com.wayi.arknights",
	}
	pkg, ok := byClient[client]
	if !ok {
		pkg = "com.hypergryph.arknights"
	}
	out, _ := execOut(20*time.Second, cfg.AdbExe, "-s", "127.0.0.1:"+port, "shell", "pm", "list", "packages")
	hits := regexp.MustCompile(`(?i)package:(com\.[a-z0-9._]*arknights[a-z0-9._]*)`).FindAllStringSubmatch(out, -1)
	if len(hits) == 1 && hits[0][1] != pkg {
		pkg = hits[0][1]
	}
	return pkg
}

// GameRunning 游戏进程 pid（空 = 没跑）。
func GameRunning(cfg Config, port, pkg string) string {
	out, _ := execOut(20*time.Second, cfg.AdbExe, "-s", "127.0.0.1:"+port, "shell", "pidof", pkg)
	return strings.TrimSpace(out)
}

// SweepOrphans 关掉"没人在用、也不该常驻"的模拟器。
// 背景：runner 被杀/崩时，在跑任务的模拟器不会执行停机 → 孤儿累积 → 内存被打满。
// 判据：keepEmus（常驻的 5 台）与正在跑任务的账号的实例都跳过，其余关机。
func (r *Runner) SweepOrphans() {
	keep := map[string]bool{}
	for _, id := range r.cfg.KeepEmus {
		keep[id] = true
	}
	r.mu.Lock()
	for _, t := range r.active {
		if a, ok := r.FindAccount(t.AccountID); ok {
			keep[a.Emu] = true
		}
	}
	for i := range r.accounts { // 常驻（肉鸽）机的实例永远不许当孤儿关掉
		if r.accounts[i].RogueTask != "" {
			keep[r.accounts[i].Emu] = true
		}
	}
	r.mu.Unlock()

	shut := 0
	for idx, inst := range MuMuAll(r.cfg.MumuManager) {
		if !inst.Started || idx == "" || keep[idx] {
			continue
		}
		log.Printf("[sweep] 关掉孤儿模拟器 idx=%s", idx)
		EmuShutdown(r.cfg.MumuManager, idx)
		shut++
	}
	if shut > 0 {
		log.Printf("[sweep] 共关掉 %d 个孤儿模拟器", shut)
	}
}

// DeviceHardReset：硬重启模拟器（关机 → 重开 → 等 boot → adb 连上）。
// 用于"游戏没起来/卡在登录页/黑屏"这类坏状态——重试同一个坏 VM 只会越试越糟。
func DeviceHardReset(cfg Config, emu string) {
	log.Printf("[reset] 硬重启模拟器 idx=%s", emu)
	EmuShutdown(cfg.MumuManager, emu)
	time.Sleep(8 * time.Second)
	EmuLaunch(cfg.MumuManager, emu)
	for i := 0; i < 40; i++ {
		time.Sleep(5 * time.Second)
		port := EmuPort(cfg.MumuManager, emu)
		if port == "" {
			continue
		}
		AdbConnect(cfg.AdbExe, port)
		if AdbBoot(cfg.AdbExe, port) == "1" {
			log.Printf("[reset] 模拟器 idx=%s 重启完成（boot=1 @%s）", emu, port)
			return
		}
	}
	log.Printf("[reset] 模拟器 idx=%s 重启超时（继续按普通流程试）", emu)
}
