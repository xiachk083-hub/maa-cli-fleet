// Package center —— 机队后端（中心服务）。
//
// 与旧 Python 版 API 完全兼容（同样的路径/载荷/状态文件），因此可以直接热替换：
// 现有 PowerShell 机端不用改一行就能继续上报/拉令。
//
//   POST /register  {node:{id,ver,host,machines:[...]}}      机端上线登记
//   POST /report    {node_id,kind:"heartbeat|state|event",data}
//   GET  /poll?node_id=   （长轮询 ≤25s）→ {commands:[...]}
//   POST /result    {node_id,id,ok,rc,output}
//   POST /command   {node_id|"all",cmd,machine,apply}
//   GET  /fleet     聚合视图（节点/机台/最近事件）
//   GET  /health    存活（无鉴权）
package center

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xiachk083-hub/maa-cli-fleet/internal/model"
)

// Server 是中心服务实例。
type Server struct {
	mu       sync.Mutex
	stateDir string
	token    string
	nodes    map[string]*model.Node
	pending  map[string]*model.Command
	queues   map[string]chan *model.Command
}

// New 创建中心服务；stateDir 不存在时自动建，token 缺失时自动生成。
func New(stateDir, token string) (*Server, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	s := &Server{
		stateDir: stateDir,
		token:    token,
		nodes:    map[string]*model.Node{},
		pending:  map[string]*model.Command{},
		queues:   map[string]chan *model.Command{},
	}
	if s.token == "" {
		if b, err := os.ReadFile(filepath.Join(stateDir, "token.txt")); err == nil {
			s.token = strings.TrimSpace(string(b))
		}
	}
	if s.token == "" {
		s.token = randomToken()
		_ = os.WriteFile(filepath.Join(stateDir, "token.txt"), []byte(s.token), 0o600)
	}
	s.load()
	return s, nil
}

// Token 返回当前共享密钥（部署脚本会读它）。
func (s *Server) Token() string { return s.token }

type registryFile struct {
	Nodes   map[string]*model.Node    `json:"nodes"`
	Pending map[string]*model.Command `json:"pending"`
}

func (s *Server) load() {
	b, err := os.ReadFile(filepath.Join(s.stateDir, "registry.json"))
	if err != nil {
		return
	}
	var r registryFile
	if json.Unmarshal(b, &r) == nil {
		if r.Nodes != nil {
			s.nodes = r.Nodes
		}
		if r.Pending != nil {
			s.pending = r.Pending
		}
	}
}

// save 落盘（调用方需持锁）。
func (s *Server) saveLocked() {
	b, _ := json.MarshalIndent(registryFile{Nodes: s.nodes, Pending: s.pending}, "", "  ")
	tmp := filepath.Join(s.stateDir, "registry.tmp")
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, filepath.Join(s.stateDir, "registry.json"))
	}
}

func (s *Server) event(rec map[string]any) {
	rec["ts"] = now()
	b, _ := json.Marshal(rec)
	f, err := os.OpenFile(filepath.Join(s.stateDir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func (s *Server) queueFor(nodeID string) chan *model.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.queues[nodeID]
	if !ok {
		ch = make(chan *model.Command, 64)
		s.queues[nodeID] = ch
	}
	return ch
}

func now() string { return time.Now().Format("2006-01-02 15:04:05") }

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- HTTP ------------------------------------------------------------------

// Handler 返回中心的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/fleet", s.auth(s.handleFleet))
	mux.HandleFunc("/poll", s.auth(s.handlePoll))
	mux.HandleFunc("/register", s.auth(s.handleRegister))
	mux.HandleFunc("/report", s.auth(s.handleReport))
	mux.HandleFunc("/result", s.auth(s.handleResult))
	mux.HandleFunc("/command", s.auth(s.handleCommand))
	return mux
}

// ListenAndServe 启动服务。
func (s *Server) ListenAndServe(addr string) error {
	log.Printf("[center] listening on %s（state=%s）", addr, s.stateDir)
	log.Printf("[center] token 文件：%s", filepath.Join(s.stateDir, "token.txt"))
	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fleet-Token") != s.token {
			writeJSON(w, 401, map[string]any{"ok": false, "error": "bad_token"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	n := len(s.nodes)
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "ts": now(), "nodes": n, "impl": "go"})
}

func (s *Server) handleFleet(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"ok": true, "ts": now(), "nodes": s.nodes,
		"pending": len(s.pending), "events": tailEvents(s.stateDir, 50),
	})
}

// handlePoll：机端长轮询拉指令（≤25 秒）。
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	nodeID := r.URL.Query().Get("node_id")
	if nodeID == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "node_id_required"})
		return
	}
	s.mu.Lock()
	if n, ok := s.nodes[nodeID]; ok {
		n.LastPoll = now()
	}
	s.mu.Unlock()

	ch := s.queueFor(nodeID)
	select {
	case cmd := <-ch:
		writeJSON(w, 200, map[string]any{"ok": true, "commands": []*model.Command{cmd}})
	case <-time.After(25 * time.Second):
		writeJSON(w, 200, map[string]any{"ok": true, "commands": []any{}})
	case <-r.Context().Done():
	}
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "bad_body"})
		return
	}
	var req struct {
		Node *model.Node `json:"node"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Node == nil || req.Node.ID == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "node.id_required"})
		return
	}
	s.mu.Lock()
	prev := s.nodes[req.Node.ID]
	if prev != nil { // 保留已有状态，合并新信息
		req.Node.State = prev.State
		if req.Node.Host == "" {
			req.Node.Host = prev.Host
		}
	}
	req.Node.LastSeen = now()
	req.Node.Online = true
	s.nodes[req.Node.ID] = req.Node
	s.saveLocked()
	s.mu.Unlock()
	s.event(map[string]any{"kind": "register", "node_id": req.Node.ID, "ver": req.Node.Ver, "host": req.Node.Host})
	writeJSON(w, 200, map[string]any{"ok": true, "ts": now()})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "bad_body"})
		return
	}
	var req struct {
		NodeID string          `json:"node_id"`
		Kind   string          `json:"kind"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.NodeID == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "node_id_required"})
		return
	}
	s.mu.Lock()
	n, ok := s.nodes[req.NodeID]
	if !ok {
		n = &model.Node{ID: req.NodeID}
		s.nodes[req.NodeID] = n
	}
	n.LastSeen = now()
	n.Online = true
	switch req.Kind {
	case "state":
		_ = json.Unmarshal(req.Data, &n.State)
	case "heartbeat":
		n.Heartbeat = &model.Heartbeat{TS: now(), Data: req.Data}
	}
	s.saveLocked()
	s.mu.Unlock()

	ev := map[string]any{"kind": req.Kind, "node_id": req.NodeID}
	if req.Kind == "event" {
		var d any
		_ = json.Unmarshal(req.Data, &d)
		ev["data"] = d
	}
	s.event(ev)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	body, _ := readBody(r)
	var req struct {
		NodeID string `json:"node_id"`
		ID     string `json:"id"`
		OK     bool   `json:"ok"`
		RC     int    `json:"rc"`
		Output string `json:"output"`
	}
	_ = json.Unmarshal(body, &req)
	s.mu.Lock()
	if c, ok := s.pending[req.ID]; ok {
		out := req.Output
		if len(out) > 4000 {
			out = out[:4000]
		}
		c.Result = &model.Result{OK: req.OK, RC: req.RC, Output: out, TS: now()}
		c.Done = true
	}
	s.saveLocked()
	s.mu.Unlock()
	s.event(map[string]any{"kind": "result", "node_id": req.NodeID, "cmd_id": req.ID, "ok": req.OK, "rc": req.RC})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	body, _ := readBody(r)
	var req struct {
		NodeID  string `json:"node_id"`
		Cmd     string `json:"cmd"`
		Machine string `json:"machine"`
		Apply   bool   `json:"apply"`
		Kind    string `json:"kind"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Cmd == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "cmd_required"})
		return
	}
	target := req.NodeID
	if target == "" {
		target = "all"
	}
	s.mu.Lock()
	var targets []string
	if target == "all" {
		for id := range s.nodes {
			targets = append(targets, id)
		}
	} else {
		targets = []string{target}
	}
	s.mu.Unlock()

	queued := make([]string, 0, len(targets))
	for _, t := range targets {
		cmd := &model.Command{
			ID: model.NewID(), TS: now(), Cmd: req.Cmd, Machine: req.Machine,
			Apply: req.Apply, Kind: req.Kind,
		}
		s.mu.Lock()
		s.pending[cmd.ID] = cmd
		s.saveLocked()
		s.mu.Unlock()
		s.queueFor(t) <- cmd
		s.event(map[string]any{"kind": "command", "node_id": t, "cmd": cmd})
		queued = append(queued, cmd.ID)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "queued": queued, "targets": targets})
}

// ---- 工具 ------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	if len(buf) == 0 {
		return nil, fmt.Errorf("empty")
	}
	return buf, nil
}

func tailEvents(stateDir string, n int) []map[string]any {
	b, err := os.ReadFile(filepath.Join(stateDir, "events.jsonl"))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]map[string]any, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(ln), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}
