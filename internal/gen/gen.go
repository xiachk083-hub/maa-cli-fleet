// Package gen —— 从 AUTO-MAS 配置生成我们的账号表与任务文件（Go 版，取代 PS 生成器）。
//
// 读 E:\AUTO-MAS\config\ScriptConfig.json，写：
//   runner/accounts.json            账号表（id/名字/服务器/关卡/剿灭/实例）
//   config/tasks/daily_<id>.toml    日常任务（6 段链）
//   config/tasks/ann_<id>.toml      剿灭作战（仅开启的）
//   config/profiles/<client>.toml   每服务器一份 profile
// 凭据（手机号/邮箱）一律不落盘。
package gen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xiachk083-hub/maa-cli-fleet/internal/model"
)

type autoMasScript struct {
	Emulator struct {
		Index string `json:"Index"`
	} `json:"Emulator"`
	Info struct {
		Name string `json:"Name"`
	} `json:"Info"`
	SubConfigs struct {
		UserData struct {
			Instances []struct {
				UID string `json:"uid"`
			} `json:"instances"`
		} `json:"UserData"`
	} `json:"SubConfigsInfo"`
}

type autoMasUser struct {
	Info struct {
		Name          string `json:"Name"`
		Server        string `json:"Server"`
		Stage         string `json:"Stage"`
		StageMode     string `json:"StageMode"`
		Annihilation  string `json:"Annihilation"`
		MedicineNumb  int    `json:"MedicineNumb"`
		SeriesNumb    string `json:"SeriesNumb"`
	} `json:"Info"`
	Task struct {
		IfStartUp bool `json:"IfStartUp"`
		IfFight   bool `json:"IfFight"`
	} `json:"Task"`
}

// Run 生成产物；ourIDs 是已接管的 id（跳过）。
func Run(autoMasDir, root string, ourIDs []string, dryRun bool) error {
	// 官服的"切换账号"掩码（主机本地文件，不进库）：{"a04":"186****6119", ...}
	masks := map[string]string{}
	if b, err := os.ReadFile(filepath.Join(root, "runner", "masks.json")); err == nil {
		_ = json.Unmarshal(b, &masks)
	}
	fmt.Printf("官服掩码载入：%d 个
", len(masks))

	raw, err := os.ReadFile(filepath.Join(autoMasDir, "config", "ScriptConfig.json"))
	if err != nil {
		return fmt.Errorf("读 ScriptConfig.json 失败：%w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	skip := map[string]bool{}
	for _, id := range ourIDs {
		skip[id] = true
	}

	var accounts []model.Account
	for key, val := range top {
		if key == "instances" {
			continue
		}
		var sc autoMasScript
		if json.Unmarshal(val, &sc) != nil || sc.Emulator.Index == "" {
			continue
		}
		// 取该脚本下第一个用户的数据
		var holder struct {
			Sub struct {
				UserData json.RawMessage `json:"UserData"`
			} `json:"SubConfigsInfo"`
		}
		if json.Unmarshal(val, &holder) != nil {
			continue
		}
		var ud struct {
			Instances []struct {
				UID string `json:"uid"`
			} `json:"instances"`
		}
		if json.Unmarshal(holder.Sub.UserData, &ud) != nil || len(ud.Instances) == 0 {
			continue
		}
		var users map[string]json.RawMessage
		if json.Unmarshal(holder.Sub.UserData, &users) != nil {
			continue
		}
		userRaw, ok := users[ud.Instances[0].UID]
		if !ok {
			continue
		}
		var u autoMasUser
		if json.Unmarshal(userRaw, &u) != nil {
			continue
		}
		id := "a" + padLeft(sc.Emulator.Index, 2)
		if skip[id] || (!u.Task.IfStartUp && !u.Task.IfFight) {
			continue
		}
		ann := ""
		annTask := ""
		if u.Info.Annihilation != "" && !strings.EqualFold(u.Info.Annihilation, "Close") {
			ann = "龙门市区"
			annTask = "ann_" + id
		}
		accounts = append(accounts, model.Account{
			ID: id, Name: u.Info.Name, Client: u.Info.Server, Stage: u.Info.Stage,
			Mode: u.Info.StageMode, Fallback: "1-7", Ann: ann, Emu: sc.Emulator.Index,
			Daily: "daily_" + id, AnnTask: annTask, State: "state_" + id, Enabled: true,
		})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })

	byClient := map[string]int{}
	annN := 0
	for _, a := range accounts {
		byClient[a.Client]++
		if a.AnnTask != "" {
			annN++
		}
	}
	fmt.Printf("账号数（待迁）：%d\n", len(accounts))
	for k, v := range byClient {
		fmt.Printf("  %s=%d\n", k, v)
	}
	fmt.Printf("剿灭开启：%d\n", annN)
	if dryRun {
		for _, a := range accounts[:min(len(accounts), 6)] {
			fmt.Printf("  %s %s %s stage=%s emu=%s ann=%s\n", a.ID, a.Name, a.Client, a.Stage, a.Emu, a.Ann)
		}
		return nil
	}

	tasksDir := filepath.Join(root, "config", "tasks")
	profDir := filepath.Join(root, "config", "profiles")
	runnerDir := filepath.Join(root, "runner")
	for _, d := range []string{tasksDir, profDir, runnerDir} {
		_ = os.MkdirAll(d, 0o755)
	}
	for _, a := range accounts {
		body := fmt.Sprintf("# %s · %s · %s · 刷 %s（由 fleet gen 从 AUTO-MAS 配置生成）\n", a.ID, a.Name, a.Client, a.Stage) +
			dailyToml(a.Client, a.Stage)
		_ = os.WriteFile(filepath.Join(tasksDir, "daily_"+a.ID+".toml"), []byte(body), 0o644)
		if a.AnnTask != "" {
			ab := fmt.Sprintf("# %s · %s · 剿灭作战 %s\n", a.ID, a.Name, a.Ann) +
				fmt.Sprintf("[[tasks]]\nname = \"剿灭作战\"\ntype = \"Fight\"\nparams = { stage = \"%s\", medicine = 0, stone = 0, series = 0 }\n", a.Ann)
			_ = os.WriteFile(filepath.Join(tasksDir, "ann_"+a.ID+".toml"), []byte(ab), 0o644)
		}
	}
	for cl := range byClient {
		p := filepath.Join(profDir, cl+".toml")
		if _, err := os.Stat(p); err == nil {
			continue
		}
		_ = os.WriteFile(p, []byte(profileToml(cl)), 0o644)
	}
	af := model.AccountsFile{GeneratedAt: nowISO(), Slots: 8, Accounts: accounts}
	b, _ := json.MarshalIndent(af, "", "  ")
	_ = os.WriteFile(filepath.Join(runnerDir, "accounts.json"), b, 0o644)
	fmt.Printf("已写：%d 份日常 + %d 份剿灭 + profile %d 份 + accounts.json\n", len(accounts), annN, len(byClient))
	return nil
}

func dailyToml(client, stage string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[[tasks]]\nname = \"开始唤醒\"\ntype = \"StartUp\"\nparams = { client_type = \"%s\", start_game_enabled = true }\n\n", client)
	fmt.Fprintf(&sb, "[[tasks]]\nname = \"刷理智\"\ntype = \"Fight\"\nparams = { stage = \"%s\", medicine = 0, stone = 0, series = 0 }\n\n", stage)
	// 第二步：剩余理智——主关卡单次消耗大，零头用低消耗关卡(1-7)榨干；
	// 主关卡不可用时（活动关关闭）也是靠它把理智清完。
	sb.WriteString("[[tasks]]\nname = \"刷剩余理智\"\ntype = \"Fight\"\nparams = { stage = \"1-7\", medicine = 0, stone = 0, series = 0 }\n\n")
	sb.WriteString("[[tasks]]\nname = \"公开招募\"\ntype = \"Recruit\"\nparams = { refresh = true, select = [4, 5], confirm = [3, 4], times = 4, skip_robot = true }\n\n")
	sb.WriteString("[[tasks]]\nname = \"基建换班\"\ntype = \"Infrast\"\nparams = { facility = [\"Mfg\",\"Trade\",\"Power\",\"Control\",\"Reception\",\"Office\",\"Dorm\"], drones = \"Money\", threshold = 0.3, dorm_trust_enabled = true }\n\n")
	sb.WriteString("[[tasks]]\nname = \"信用购物\"\ntype = \"Mall\"\nparams = { visit_friends = true, shopping = true, buy_first = [\"招聘许可\"], blacklist = [\"加急许可\",\"家具零件\"] }\n\n")
	sb.WriteString("[[tasks]]\nname = \"领取奖励\"\ntype = \"Award\"\nparams = { award = true, mail = true }\n")
	return sb.String()
}

func profileToml(client string) string {
	return "[connection]\n" +
		"adb_path = \"E:/MuMu Player 12/shell/adb.exe\"\n" +
		"address = \"127.0.0.1:16384\"\n" +
		"config = \"General\"\n\n" +
		"[resource]\n" +
		fmt.Sprintf("global_resource = \"%s\"\n", globalResource(client)) +
		"user_resource = false\n\n" +
		"[instance_options]\n" +
		"touch_mode = \"MaaTouch\"\n"
}

// globalResource：国服（官服/B服）没有 global 覆盖目录，统一用 Official；其余用自身名字。
func globalResource(client string) string {
	if client == "Bilibili" || client == "Official" || client == "" {
		return "Official"
	}
	return client
}

func padLeft(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s
}

func nowISO() string { return time.Now().Format("2006-01-02T15:04:05") }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
