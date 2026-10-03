package center

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// 机端认的指令。写操作在机端侧默认 dry-run，apply=true 才真执行。
var mcpCmds = map[string]bool{
	"status": true, "daily": true, "rogue": true, "chain": true,
	"cycle": true, "cycle-stop": true, "watch": true, "watch-stop": true,
	"recover": true, "fix": true, "fix1": true, "fix2": true, "fix3": true,
	"run": true,
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// handleMCP 是无状态的 Streamable HTTP。只接受 POST，请求用 JSON 回应，
// 不升级成 SSE（Cloudflare 快速隧道不支持 SSE）。
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = r.Body.Close()
	if err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	if body[0] == '[' {
		var msgs []rpcMessage
		if json.Unmarshal(body, &msgs) != nil {
			writeRPCError(w, nil, -32700, "parse error")
			return
		}
		var out []rpcResponse
		for _, m := range msgs {
			if res, ok := s.dispatchMCP(m); ok {
				out = append(out, res)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	var m rpcMessage
	if json.Unmarshal(body, &m) != nil || m.JSONRPC != "2.0" {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}
	res, ok := s.dispatchMCP(m)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) dispatchMCP(m rpcMessage) (rpcResponse, bool) {
	if len(m.ID) == 0 {
		return rpcResponse{}, false
	}
	res := rpcResponse{JSONRPC: "2.0", ID: m.ID}
	switch m.Method {
	case "initialize":
		res.Result = map[string]any{
			"protocolVersion": negotiateProtocol(m.Params),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "maa-cli-fleet", "version": "0.1.0"},
		}
	case "ping":
		res.Result = map[string]any{}
	case "tools/list":
		res.Result = map[string]any{"tools": mcpToolSchemas()}
	case "tools/call":
		res.Result = s.mcpCall(m.Params)
	default:
		res.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	return res, true
}

func negotiateProtocol(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	switch p.ProtocolVersion {
	case "2024-11-05", "2025-03-26", "2025-06-18":
		return p.ProtocolVersion
	default:
		return "2025-03-26"
	}
}

func mcpToolSchemas() []map[string]any {
	cmdEnum := make([]string, 0, len(mcpCmds))
	for c := range mcpCmds {
		cmdEnum = append(cmdEnum, c)
	}
	obj := func(desc string, props map[string]any, required []string) map[string]any {
		schema := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			schema["required"] = required
		}
		return map[string]any{
			"name":        "",
			"description": desc,
			"inputSchema": schema,
		}
	}
	view := obj("读取中心当前台账：节点、各机状态、未完成指令数、最近事件。只读。", map[string]any{}, nil)
	view["name"] = "fleet_view"
	cmd := obj("向机端排队一条指令。写操作（daily/rogue/cycle 等）在机端默认 dry-run，apply=true 才真执行。机端没在拉令时，指令会停在队列里。", map[string]any{
		"node_id": map[string]any{"type": "string", "description": "机端 id，例如 host-mrfz0000。空字符串表示全部已登记节点。"},
		"cmd":     map[string]any{"type": "string", "enum": cmdEnum, "description": "status 只读；其余为写操作。"},
		"machine": map[string]any{"type": "string", "description": "机台名，例如 l-1。status 可省略。"},
		"kind":    map[string]any{"type": "string", "description": "可选，daily 或 rogue。"},
		"apply":   map[string]any{"type": "boolean", "description": "默认 false。true 才让机端真的执行写操作。", "default": false},
	}, []string{"cmd"})
	cmd["name"] = "fleet_command"
	return []map[string]any{view, cmd}
}

func (s *Server) mcpCall(params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil || p.Name == "" {
		return mcpText(false, map[string]any{"ok": false, "error": "name_required"})
	}
	switch p.Name {
	case "fleet_view":
		return mcpText(true, s.snapshot())
	case "fleet_command":
		return mcpText(true, s.mcpCommand(p.Arguments))
	default:
		return mcpText(false, map[string]any{"ok": false, "error": "unknown_tool"})
	}
}

func (s *Server) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"ok": true, "ts": now(), "nodes": s.nodes,
		"pending": len(s.pending), "events": tailEvents(s.stateDir, 50),
	}
}

func (s *Server) mcpCommand(raw json.RawMessage) map[string]any {
	var a struct {
		NodeID  string `json:"node_id"`
		Cmd     string `json:"cmd"`
		Machine string `json:"machine"`
		Kind    string `json:"kind"`
		Apply   bool   `json:"apply"`
	}
	if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &a) != nil {
		return map[string]any{"ok": false, "error": "bad_arguments"}
	}
	if !mcpCmds[a.Cmd] {
		return map[string]any{"ok": false, "error": "unknown_cmd"}
	}
	queued, targets := s.enqueue(a.NodeID, a.Cmd, a.Machine, a.Kind, a.Apply)
	if len(targets) == 0 {
		return map[string]any{"ok": false, "error": "no_nodes", "note": "中心还没有任何机端登记"}
	}
	note := "已入队。机端下次拉取时执行。"
	if !a.Apply && a.Cmd != "status" {
		note = "已入队。机端会按 dry-run 回执，不会真的改任务。要执行需 apply=true。"
	}
	return map[string]any{"ok": true, "queued": queued, "targets": targets, "apply": a.Apply, "note": note}
}

func mcpText(ok bool, v any) map[string]any {
	b, _ := json.MarshalIndent(v, "", "  ")
	out := map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
	}
	if !ok {
		out["isError"] = true
	}
	return out
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, 200, rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	})
}
