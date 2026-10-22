package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiachk083-hub/maa-cli-fleet/internal/model"
)

func newSwitchTestRunner(t *testing.T) (*Runner, string, *rogueLane) {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	tasksDir := filepath.Join(cfgDir, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rogue_old", "rogue_new"} {
		if err := os.WriteFile(filepath.Join(tasksDir, name+".toml"), []byte("[[tasks]]\nname = \"x\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	accPath := filepath.Join(dir, "runner", "accounts.json")
	if err := os.MkdirAll(filepath.Dir(accPath), 0o755); err != nil {
		t.Fatal(err)
	}
	accJSON := `{"generatedAt":"2026-10-22","accounts":[{"id":"l7","name":"x","rogueTask":"rogue_old","enabled":true,"custom":"keep"}]}`
	if err := os.WriteFile(accPath, []byte(accJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Runner{
		cfg: Config{
			AccountsFile: accPath,
			ConfigDir:    cfgDir,
			StateFile:    filepath.Join(dir, "runner", "state.json"),
		},
		accounts: []model.Account{{ID: "l7", Name: "x", RogueTask: "rogue_old", Enabled: true}},
		state:    &State{Tasks: map[string]*Task{}},
		lanes:    map[string]*rogueLane{},
	}
	r.state.Tasks["rogue:l7"] = &Task{Key: "rogue:l7", AccountID: "l7", Kind: "rogue", File: "rogue_old"}
	lane := &rogueLane{acc: r.accounts[0], task: "rogue_old", note: "常驻"}
	r.lanes["l7"] = lane
	return r, accPath, lane
}

func TestSetRogueUpdatesLaneAndAccounts(t *testing.T) {
	r, accPath, lane := newSwitchTestRunner(t)

	if err := r.SetRogue("l7", "rogue_new.toml"); err != nil {
		t.Fatalf("SetRogue: %v", err)
	}
	if got := r.accounts[0].RogueTask; got != "rogue_new" {
		t.Fatalf("in-memory rogueTask = %q, want rogue_new", got)
	}
	if got := lane.taskName(); got != "rogue_new" {
		t.Fatalf("lane task = %q, want rogue_new", got)
	}
	if got := r.state.Tasks["rogue:l7"].File; got != "rogue_new" {
		t.Fatalf("state file = %q, want rogue_new", got)
	}

	b, err := os.ReadFile(accPath)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	var accounts []map[string]json.RawMessage
	if err := json.Unmarshal(top["accounts"], &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts len = %d", len(accounts))
	}
	if got := string(accounts[0]["rogueTask"]); got != `"rogue_new"` {
		t.Fatalf("persisted rogueTask = %s", got)
	}
	if got := string(accounts[0]["custom"]); got != `"keep"` {
		t.Fatalf("unknown field not preserved: %s", got)
	}
	backups, _ := filepath.Glob(accPath + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %d, want 1", len(backups))
	}
}

func TestSetRogueRejectsMissingTask(t *testing.T) {
	r, accPath, lane := newSwitchTestRunner(t)
	before, _ := os.ReadFile(accPath)

	if err := r.SetRogue("l7", "rogue_missing"); err == nil {
		t.Fatal("SetRogue(missing) should fail")
	}
	after, _ := os.ReadFile(accPath)
	if string(before) != string(after) {
		t.Fatal("accounts.json changed on failed switch")
	}
	if got := lane.taskName(); got != "rogue_old" {
		t.Fatalf("lane task = %q, want rogue_old", got)
	}
}

func TestSetRogueRejectsNoLane(t *testing.T) {
	r, _, _ := newSwitchTestRunner(t)
	r.accounts = append(r.accounts, model.Account{ID: "a1", Enabled: true})
	if err := r.SetRogue("a1", "rogue_new"); err == nil {
		t.Fatal("SetRogue(no lane) should fail")
	}
}

func TestSetRogueRejectsBadNames(t *testing.T) {
	r, _, _ := newSwitchTestRunner(t)
	for _, bad := range []string{"", "../rogue_new", "a/b", `a\b`, "a:b"} {
		if err := r.SetRogue("l7", bad); err == nil {
			t.Fatalf("SetRogue(%q) should fail", bad)
		}
	}
}

func TestSetRogueSameTaskIsNoop(t *testing.T) {
	r, accPath, lane := newSwitchTestRunner(t)
	if err := r.SetRogue("l7", "rogue_old"); err != nil {
		t.Fatal(err)
	}
	if got := lane.taskName(); got != "rogue_old" {
		t.Fatalf("lane task = %q", got)
	}
	backups, _ := filepath.Glob(accPath + ".bak-*")
	if len(backups) != 0 {
		t.Fatalf("same-task switch created %d backups", len(backups))
	}
}

func TestQueueOpSetRoguePayload(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	if err := QueueOp(state, "setRogue", "l7:rogue_new"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "queue.in"))
	if err != nil {
		t.Fatal(err)
	}
	var op Op
	if err := json.Unmarshal(b, &op); err != nil {
		t.Fatal(err)
	}
	if op.Op != "setRogue" || op.Account != "l7" || op.RogueTask != "rogue_new" {
		t.Fatalf("unexpected op: %+v", op)
	}
}

func TestApplyOpsSetRogue(t *testing.T) {
	r, _, lane := newSwitchTestRunner(t)
	q := filepath.Join(filepath.Dir(r.cfg.StateFile), "queue.in")
	if err := os.MkdirAll(filepath.Dir(q), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(q, []byte(`{"op":"setRogue","account":"l7","rogueTask":"rogue_new"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := r.ApplyOps(); n != 1 {
		t.Fatalf("ApplyOps applied %d, want 1", n)
	}
	if got := lane.taskName(); got != "rogue_new" {
		t.Fatalf("lane task = %q, want rogue_new", got)
	}
	if _, err := os.Stat(q); !os.IsNotExist(err) {
		t.Fatalf("queue.in should be removed, err=%v", err)
	}
}

func TestSetRogueRunningLaneMarksSwitch(t *testing.T) {
	r, _, lane := newSwitchTestRunner(t)
	lane.run = &maaRun{}
	if err := r.SetRogue("l7", "rogue_new"); err != nil {
		t.Fatal(err)
	}
	if !lane.takeSwitch() {
		t.Fatal("switching a running lane should mark the switch flag")
	}
	if got := lane.taskName(); got != "rogue_new" {
		t.Fatalf("lane task = %q", got)
	}
}

func TestRogueLaneSwitchFlags(t *testing.T) {
	l := &rogueLane{acc: model.Account{RogueTask: "old"}}
	if got := l.taskName(); got != "old" {
		t.Fatalf("taskName = %q", got)
	}
	if run := l.switchTask("new"); run != nil {
		t.Fatal("switchTask should not return a run when idle")
	}
	if l.takeSwitch() {
		t.Fatal("idle switch should not mark switching")
	}
	l.markSwitch()
	if !l.takeSwitch() {
		t.Fatal("markSwitch should be consumed")
	}
	if l.takeSwitch() {
		t.Fatal("switch flag should be consumed once")
	}
	if !strings.Contains(l.taskName(), "new") {
		t.Fatalf("taskName = %q", l.taskName())
	}
}

func TestRogueLaneSwitchWithRun(t *testing.T) {
	run := &maaRun{}
	l := &rogueLane{acc: model.Account{RogueTask: "old"}, run: run}
	if got := l.switchTask("new"); got != run {
		t.Fatal("switchTask should return the running maa")
	}
	if !l.takeSwitch() {
		t.Fatal("switching a running lane should set the switch flag")
	}
	if got := l.taskName(); got != "new" {
		t.Fatalf("taskName = %q", got)
	}
}
