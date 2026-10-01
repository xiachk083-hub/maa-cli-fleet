// Package env —— 机端的"模拟器环境管理"（自管环境的一部分）。
//
// 设计原则（血的教训）：一次只动一台、一项，改完必须验证；绝不批量盲改。
//
//	fleet env show    -emu 2                 看某台当前设置
//	fleet env set     -emu 2 -key max_frame_rate -value 30   [单台单键]
//	fleet env restart -emu 2                 关机重开（让设置生效）
//	fleet env converge -spec env/desired.json [-only 1,2,3]  按目标规格收敛（逐台+核对）
//	fleet env check   -spec env/desired.json 只报差异，不动手
package env

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Cfg 环境管理配置。
type Cfg struct {
	MuMuManager string `json:"mumuManager"`
	Spec        string `json:"spec"` // 目标规格 json 路径
}

// Spec 目标规格：default 全实例生效；perInstance 覆盖。
type Spec struct {
	Default     map[string]string            `json:"default"`
	PerInstance map[string]map[string]string `json:"perInstance"`
}

var reKV = regexp.MustCompile(`"([^"]+)":\s*"([^"]*)"`)

func run(timeout time.Duration, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	go func() { out, _ = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
	}
	return string(out)
}

// Show 打印某实例的全部可写设置。
func Show(c Cfg, emu string) error {
	raw := run(60*time.Second, c.MuMuManager, "setting", "-v", emu, "--all_writable")
	kv := map[string]string{}
	for _, m := range reKV.FindAllStringSubmatch(raw, -1) {
		kv[m[1]] = m[2]
	}
	if len(kv) == 0 {
		return fmt.Errorf("没读到实例 %s 的设置", emu)
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	// 只打印关心的几类
	interesting := []string{"performance_mem.custom", "performance_cpu.custom", "performance_mode",
		"max_frame_rate", "dynamic_adjust_frame_rate", "resolution_width.custom", "resolution_height.custom",
		"resolution_mode", "renderer_mode", "gpu_mode", "player_name"}
	for _, k := range interesting {
		if v, ok := kv[k]; ok {
			log.Printf("  %-28s = %s", k, v)
		}
	}
	log.Printf("  （共 %d 项可写设置）", len(kv))
	return nil
}

// SetOne 改单个键（单台）。返回旧值，方便回滚。
func SetOne(c Cfg, emu, key, value string) (string, error) {
	old := getOne(c, emu, key)
	if out := run(60*time.Second, c.MuMuManager, "setting", "-v", emu, "--key", key, "--value", value); strings.Contains(out, "error") {
		return old, fmt.Errorf("设置失败：%s", strings.TrimSpace(out))
	}
	now := getOne(c, emu, key)
	if now != value {
		return old, fmt.Errorf("设置未生效：现在仍是 %s", now)
	}
	log.Printf("[env] 实例 %s：%s %s → %s", emu, key, old, now)
	return old, nil
}

func getOne(c Cfg, emu, key string) string {
	raw := run(60*time.Second, c.MuMuManager, "setting", "-v", emu, "--key", key, "--all_writable")
	m := reKV.FindStringSubmatch(raw)
	if len(m) == 3 {
		return m[2]
	}
	return ""
}

// Restart 关机重开（设置要重启才生效）。
func Restart(c Cfg, emu string) {
	run(60*time.Second, c.MuMuManager, "control", "--vmindex", emu, "shutdown")
	time.Sleep(3 * time.Second)
	run(60*time.Second, c.MuMuManager, "control", "--vmindex", emu, "launch")
	log.Printf("[env] 实例 %s 已重开", emu)
}

// Check 只报差异。
func Check(c Cfg, emus []string) (int, error) {
	spec, err := loadSpec(c.Spec)
	if err != nil {
		return 0, err
	}
	diff := 0
	for _, emu := range emus {
		want := spec.defaultFor(emu)
		for k, v := range want {
			cur := getOne(c, emu, k)
			if cur != v && cur != "" {
				log.Printf("  实例 %s：%s 现在=%s 期望=%s", emu, k, cur, v)
				diff++
			}
		}
	}
	log.Printf("[env] 差异 %d 项", diff)
	return diff, nil
}

// Converge 按规格收敛（逐台：改 → 重启 → 复核）。
func Converge(c Cfg, emus []string) error {
	spec, err := loadSpec(c.Spec)
	if err != nil {
		return err
	}
	for _, emu := range emus {
		want := spec.defaultFor(emu)
		changed := false
		for k, v := range want {
			cur := getOne(c, emu, k)
			if cur == "" || cur == v {
				continue
			}
			if _, err := SetOne(c, emu, k, v); err != nil {
				log.Printf("[env] 实例 %s %s 失败：%v", emu, k, err)
				continue
			}
			changed = true
		}
		if changed {
			Restart(c, emu)
			// 重启后复核
			for k, v := range want {
				if cur := getOne(c, emu, k); cur != v {
					log.Printf("[env] 实例 %s 重启后 %s=%s（期望 %s）✗", emu, k, cur, v)
				}
			}
		}
	}
	return nil
}

func (s Spec) defaultFor(emu string) map[string]string {
	out := map[string]string{}
	for k, v := range s.Default {
		out[k] = v
	}
	if o, ok := s.PerInstance[emu]; ok {
		for k, v := range o {
			out[k] = v
		}
	}
	return out
}

func loadSpec(path string) (Spec, error) {
	var s Spec
	if path == "" {
		path = "env/desired.json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("读规格失败 %s：%w", path, err)
	}
	b = []byte(strings.TrimPrefix(string(b), "\ufeff"))
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	return s, nil
}
