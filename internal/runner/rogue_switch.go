package runner

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiachk083-hub/maa-cli-fleet/internal/model"
)

// ---- 肉鸽任务热切换（setRogue）-----------------------------------------------
//
// 只换一个账号的肉鸽任务：更新内存里的车道任务 → 持久化 accounts.json →
// 杀掉当前 maa（如果有）→ 车道下一轮用新任务重发。不重启 runner，不影响其他车道。
//
// 用法：
//
//	fleet runner -set-rogue l7:rogue_jiegarden_l7
//
// 注意：换主题前要先结算/放弃当前主题里进行中的探索；MAA 不会自己处理这个。

// taskName 返回车道当前任务（热切换后以 task 为准；老测试构造的 lane 可能只填 acc）。
func (l *rogueLane) taskName() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.task != "" {
		return l.task
	}
	return l.acc.RogueTask
}

// switchTask 更新任务名；如果当前有 maa 在跑，标记 switching 并把 run 返回给调用方 kill。
func (l *rogueLane) switchTask(name string) *maaRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.task = name
	if l.run != nil {
		l.switching = true
	}
	return l.run
}

// markSwitch：起机竞态用——刚 setRun 完才发现任务已经变了。
func (l *rogueLane) markSwitch() {
	l.mu.Lock()
	l.switching = true
	l.mu.Unlock()
}

// takeSwitch 消费一次切换标记（通道退出后调用）。
func (l *rogueLane) takeSwitch() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.switching {
		l.switching = false
		return true
	}
	return false
}

// SetRogue 热切换某账号的肉鸽任务。只允许现有常驻车道；任务文件必须已存在。
func (r *Runner) SetRogue(accountID, task string) error {
	accountID = strings.TrimSpace(accountID)
	task, err := normalizeTaskName(task)
	if err != nil {
		return err
	}
	if r.cfg.ConfigDir == "" {
		return fmt.Errorf("未配置 configDir")
	}
	taskPath := filepath.Join(r.cfg.ConfigDir, "tasks", task+".toml")
	info, err := os.Stat(taskPath)
	if err != nil {
		return fmt.Errorf("任务文件不存在：%s（%w）", taskPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("任务路径是目录：%s", taskPath)
	}
	if info.Size() == 0 {
		return fmt.Errorf("任务文件为空：%s", taskPath)
	}

	r.mu.Lock()
	idx := -1
	for i := range r.accounts {
		if r.accounts[i].ID == accountID {
			idx = i
			break
		}
	}
	if idx < 0 {
		r.mu.Unlock()
		return fmt.Errorf("没有这个账号：%s", accountID)
	}
	acc := r.accounts[idx]
	lane := r.lanes[accountID]
	r.mu.Unlock()

	if !acc.Enabled {
		return fmt.Errorf("账号 %s 已停用", accountID)
	}
	if lane == nil {
		return fmt.Errorf("账号 %s 没有常驻肉鸽车道（先配置 rogueTask 并重启 runner）", accountID)
	}
	if lane.stopped() {
		return fmt.Errorf("账号 %s 的肉鸽车道已停止", accountID)
	}

	oldTask := lane.taskName()
	if oldTask == task {
		log.Printf("[rogue:%s] 已是任务 %s，跳过热切换", accountID, task)
		return nil
	}

	if err := updateAccountsRogueTask(r.cfg.AccountsFile, accountID, task); err != nil {
		return fmt.Errorf("写 accounts.json 失败：%w", err)
	}

	r.mu.Lock()
	r.accounts[idx].RogueTask = task
	if r.state != nil && r.state.Tasks != nil {
		if t := r.state.Tasks["rogue:"+accountID]; t != nil {
			t.File = task
			t.Note = "热切换：" + task
		}
	}
	r.mu.Unlock()

	run := lane.switchTask(task)
	log.Printf("[rogue:%s] 热切换任务 %s → %s", accountID, oldTask, task)
	if run != nil && run.cmd != nil && run.cmd.Process != nil {
		log.Printf("[rogue:%s] 停止旧任务 maa pid=%d，等待车道重发", accountID, run.cmd.Process.Pid)
		killMaa(run)
	}
	return nil
}

// normalizeTaskName 只接受简单任务名（可带 .toml 后缀），防止路径穿越。
func normalizeTaskName(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".toml")
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\:") {
		return "", fmt.Errorf("非法任务名：%q", s)
	}
	return s, nil
}

// updateAccountsRogueTask 只改目标账号的 rogueTask，保留 accounts.json 里的其他字段与顺序。
func updateAccountsRogueTask(path, accountID, task string) error {
	if path == "" {
		return fmt.Errorf("未配置 accountsFile")
	}
	b, err := model.ReadFileBOM(path)
	if err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return fmt.Errorf("解析 accounts.json：%w", err)
	}
	raw, ok := top["accounts"]
	if !ok {
		return fmt.Errorf("accounts.json 缺 accounts 字段")
	}
	var accounts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &accounts); err != nil {
		return fmt.Errorf("解析 accounts 数组：%w", err)
	}

	found := false
	for _, a := range accounts {
		var id string
		if json.Unmarshal(a["id"], &id) == nil && id == accountID {
			a["rogueTask"] = rawJSONString(task)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("accounts.json 里没有账号 %s", accountID)
	}

	outAccounts, err := json.Marshal(accounts)
	if err != nil {
		return err
	}
	top["accounts"] = outAccounts
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	bak := path + ".bak-" + time.Now().Format("20060102-150405.000")
	if err := os.WriteFile(bak, b, 0o644); err != nil {
		return fmt.Errorf("备份 accounts.json：%w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

func rawJSONString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
