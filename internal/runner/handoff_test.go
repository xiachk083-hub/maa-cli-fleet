package runner

import (
	"fmt"
	"github.com/xiachk083-hub/maa-cli-fleet/internal/model"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testRunner(t *testing.T) *Runner {
	t.Helper()
	return &Runner{cfg: Config{MaxConcurrent: 4, StateFile: filepath.Join(t.TempDir(), "state.json"), MumuManager: "fleet-test-no-device", AdbExe: "fleet-test-no-adb"}, state: &State{Tasks: map[string]*Task{}}, running: map[string]bool{}, active: map[string]*Task{}, lanes: map[string]*rogueLane{}, maaPids: map[string]int{}}
}

// The test executable acts as MuMu only in subprocesses, never contacts devices.
func TestMain(m *testing.M) {
	if os.Getenv("FLEET_TEST_MUMU") != "" {
		if len(os.Args) > 1 && os.Args[1] == "info" {
			if gate := os.Getenv("FLEET_TEST_GATE"); gate != "" {
				os.WriteFile(gate+".entered", []byte("entered"), 0600)
				for {
					if _, err := os.Stat(gate + ".release"); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			fmt.Print(`{"1":{"adb_port":1,"is_process_started":true},"2":{"is_process_started":true},"99":{"is_process_started":true}}`)
		} else {
			f, _ := os.OpenFile(os.Getenv("FLEET_TEST_MUMU"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if f != nil {
				fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
				f.Close()
				if strings.Contains(strings.Join(os.Args, " "), "sys.boot_completed") {
					fmt.Print("1")
				}
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSweepNeverTouchesDisabledOrForeignInstances(t *testing.T) {
	r := testRunner(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.MumuManager = exe
	r.accounts = []model.Account{{ID: "l1", Emu: "1", Enabled: true}, {ID: "disabled", Emu: "2", Enabled: false}}
	logFile := filepath.Join(filepath.Dir(r.cfg.StateFile), "commands.txt")
	t.Setenv("FLEET_TEST_MUMU", logFile)
	r.SweepOrphans()
	b, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "control --vmindex 1 shutdown" {
		t.Fatalf("unsafe sweep: %s", b)
	}
}

func TestPauseWaitsForDeviceUpQuiescence(t *testing.T) {
	r := testRunner(t)
	exe, _ := os.Executable()
	r.cfg.MumuManager, r.cfg.AdbExe = exe, exe
	gate := filepath.Join(filepath.Dir(r.cfg.StateFile), "gate")
	t.Setenv("FLEET_TEST_MUMU", gate+".commands")
	t.Setenv("FLEET_TEST_GATE", gate)
	a := model.Account{ID: "l1", Enabled: true, RogueTask: "rogue", Emu: "1"}
	l := &rogueLane{acc: a}
	r.lanes[a.ID] = l
	done := make(chan struct{})
	go func() { r.residentLoop(l); close(done) }()
	defer func() { l.setStop(true); os.WriteFile(gate+".release", []byte("release"), 0600); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(gate + ".entered"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("device helper not entered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	paused := make(chan struct{})
	go func() { r.pauseResident(a.ID); close(paused) }()
	select {
	case <-paused:
		t.Error("pause returned while DeviceUp still owned lane")
	case <-time.After(100 * time.Millisecond):
	}
	os.WriteFile(gate+".release", []byte("release"), 0600)
	select {
	case <-paused:
	case <-time.After(12 * time.Second):
		t.Fatal("pause did not acknowledge quiescence")
	}
}

func TestDailyFailsClosedOnResidentPauseTimeout(t *testing.T) {
	r := testRunner(t)
	exe, _ := os.Executable()
	r.cfg.MumuManager, r.cfg.AdbExe, r.cfg.MaaExe = exe, exe, exe
	r.cfg.LogDir = filepath.Dir(r.cfg.StateFile)
	commands := filepath.Join(r.cfg.LogDir, "commands")
	t.Setenv("FLEET_TEST_MUMU", commands)
	a := model.Account{ID: "l1", Enabled: true, Resident: true, RogueTask: "rogue", Emu: "1"}
	r.accounts = []model.Account{a}
	l := &rogueLane{acc: a}
	l.beginCycle()
	r.lanes[a.ID] = l
	defer l.endCycle()
	task := &Task{Key: "daily:l1", AccountID: "l1", Kind: "daily", State: "running", Attempts: 1}
	r.state.Tasks[task.Key] = task
	r.runTask(task)
	if b, _ := os.ReadFile(commands); len(b) > 0 {
		t.Fatalf("daily touched device after unacknowledged pause: %s", b)
	}
	if !l.isPaused() {
		t.Fatal("failed handoff reopened resident lane")
	}
	if task.State != "queued" || !strings.Contains(task.Note, "quiesce") {
		t.Fatalf("timeout not retryable/visible: %+v", task)
	}
}

func TestLoopPhaseTracksDailyHandoff(t *testing.T) {
	r := testRunner(t)
	a := model.Account{ID: "l1", Enabled: true, RogueTask: "rogue"}
	r.accounts = []model.Account{a}
	r.state.Tasks["rogue:l1"] = &Task{Key: "rogue:l1", AccountID: "l1", Kind: "rogue", File: "rogue"}
	r.lanes[a.ID] = &rogueLane{acc: a}

	if err := r.pauseResidentWithin(a.ID, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if got := r.state.Tasks["rogue:l1"].Phase; got != "paused" {
		t.Fatalf("pause phase = %q, want paused", got)
	}
	r.setLoopPhase(a.ID, "daily", "日常插队")
	if got := r.state.Tasks["rogue:l1"].Phase; got != "daily" {
		t.Fatalf("daily phase = %q, want daily", got)
	}
	r.resumeResident(a.ID)
	if got := r.state.Tasks["rogue:l1"].Phase; got != "rogue" {
		t.Fatalf("resume phase = %q, want rogue", got)
	}
	r.setLoopResult(a.ID, "ok")
	if got := r.state.Tasks["rogue:l1"].LastResult; got != "ok" {
		t.Fatalf("last result = %q, want ok", got)
	}
}

func TestSuccessfulDailyClearsConsumedPlan(t *testing.T) {
	r := testRunner(t)
	task := &Task{PlanAt: time.Now().Add(-time.Hour).Format(stampLayout)}
	r.scheduleNextDaily(task, model.Account{ID: "l1"})
	if task.PlanAt != "" {
		t.Fatal("consumed PlanAt would requeue daily immediately instead of returning rogue")
	}
	if due, ok := parseStamp(task.NextDue); !ok || !due.After(time.Now()) {
		t.Fatalf("invalid next cycle: %s", task.NextDue)
	}
}

func TestHandoffCyclesWaitForAllLanePhases(t *testing.T) {
	r := testRunner(t)
	l := &rogueLane{}
	r.lanes["l1"] = l
	for cycle := 0; cycle < 2; cycle++ {
		for _, phase := range []string{"DeviceUp", "forceStop", "startMaa", "Wait"} {
			if !l.beginCycle() {
				t.Fatal("rogue did not resume")
			}
			if err := r.pauseResidentWithin("l1", time.Millisecond); err == nil {
				t.Fatalf("%s not protected", phase)
			}
			if l.beginCycle() {
				t.Fatal("rogue entered while paused")
			}
			l.endCycle()
			if err := r.pauseResidentWithin("l1", time.Millisecond); err != nil {
				t.Fatal(err)
			}
			if l.beginCycle() {
				t.Fatal("rogue overlapped daily")
			}
			r.resumeResident("l1")
		}
	}
}

func TestPausedResidentKeepsHeartbeatFresh(t *testing.T) {
	r := testRunner(t)
	a := model.Account{ID: "l1", Enabled: true, RogueTask: "rogue"}
	l := &rogueLane{acc: a, paused: true}
	done := make(chan struct{})
	go func() { r.residentLoop(l); close(done) }()
	defer func() { l.setStop(true); <-done }()
	hb := filepath.Join(filepath.Dir(r.cfg.StateFile), "hb_rogue_l1.txt")
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(hb); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("paused lane never refreshed heartbeat")
}

func TestDisabledResidentsDoNotStart(t *testing.T) {
	r := testRunner(t)
	r.accounts = []model.Account{{ID: "disabled", Enabled: false, RogueTask: "rogue"}}
	r.startResidents()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.lanes) != 0 {
		t.Fatal("disabled resident started")
	}
}

func TestClaimsSerializeAccountAndConcurrentCallers(t *testing.T) {
	r := testRunner(t)
	r.accounts = []model.Account{{ID: "l1", Enabled: true, Daily: "daily", AnnTask: "ann"}, {ID: "l2", Enabled: true, Daily: "daily"}}
	for _, task := range []*Task{
		{Key: "daily:l1", AccountID: "l1", Kind: "daily", State: "queued"},
		{Key: "ann:l1", AccountID: "l1", Kind: "ann", State: "queued"},
		{Key: "daily:l2", AccountID: "l2", Kind: "daily", State: "queued"},
	} {
		r.state.Tasks[task.Key] = task
	}
	var wg sync.WaitGroup
	results := make(chan []*Task, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- r.claimTasks(time.Now()) }()
	}
	wg.Wait()
	close(results)
	counts := map[string]int{}
	for tasks := range results {
		for _, task := range tasks {
			counts[task.AccountID]++
		}
	}
	if counts["l1"] != 1 || counts["l2"] != 1 {
		t.Fatalf("duplicate/missing claims: %v", counts)
	}
}

func TestBuildPlanExcludesDisabledAndMissingAccounts(t *testing.T) {
	r := testRunner(t)
	r.accounts = []model.Account{{ID: "l1", Enabled: true, Daily: "daily", AnnTask: "ann"}, {ID: "disabled", Enabled: false}}
	for _, id := range []string{"l1", "disabled", "missing"} {
		key := "daily:" + id
		r.state.Tasks[key] = &Task{Key: key, AccountID: id, Kind: "daily", State: "queued"}
	}
	plan := r.BuildPlan()
	if len(plan) != 1 || plan[0].Account != "l1" {
		t.Fatalf("stale accounts distorted plan: %+v", plan)
	}
}

func TestRogueOnlyAccountRejectsStaleDaily(t *testing.T) {
	r := testRunner(t)
	r.accounts = []model.Account{{ID: "l5", Enabled: true, RogueTask: "rogue"}}
	r.state.Tasks["daily:l5"] = &Task{Key: "daily:l5", AccountID: "l5", Kind: "daily", State: "queued", File: "obsolete"}
	if tasks := r.claimTasks(time.Now()); len(tasks) != 0 {
		t.Fatal("rogue-only account claimed stale daily")
	}
	if plan := r.BuildPlan(); len(plan) != 0 {
		t.Fatal("rogue-only account entered daily plan")
	}
}

func TestTickRejectsStaleQueuedAccountsAndFuturePlan(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accounts []model.Account
		plan     string
	}{
		{"disabled", []model.Account{{ID: "l1", Enabled: false}}, ""},
		{"missing", nil, ""},
		{"future", []model.Account{{ID: "l1", Enabled: true, Daily: "daily"}}, time.Now().Add(time.Hour).Format(stampLayout)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testRunner(t)
			r.accounts = tc.accounts
			task := &Task{Key: "daily:l1", AccountID: "l1", Kind: "daily", State: "queued", PlanAt: tc.plan}
			r.state.Tasks[task.Key] = task
			r.Tick()
			r.mu.Lock()
			defer r.mu.Unlock()
			if task.Attempts != 0 || len(r.active) != 0 {
				t.Fatalf("ineligible task claimed: attempts=%d active=%d", task.Attempts, len(r.active))
			}
		})
	}
}
