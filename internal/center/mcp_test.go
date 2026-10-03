package center

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestMCPRoundTrip(t *testing.T) {
	srv, err := New(filepath.Join(t.TempDir(), "state"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()

	noAuth := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, noAuth)
	if rec.Code != 401 {
		t.Fatalf("no auth: %d", rec.Code)
	}

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	res := mcpPost(t, h, initBody)
	if res["error"] != nil {
		t.Fatalf("initialize: %s", recBody(res))
	}

	listed := mcpPost(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	raw, _ := json.Marshal(listed["result"])
	if !bytes.Contains(raw, []byte("fleet_view")) || !bytes.Contains(raw, []byte("fleet_command")) {
		t.Fatalf("tools: %s", raw)
	}

	note := mcpPost(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if note != nil {
		t.Fatal("notification should be 202 with empty body")
	}

	view := mcpPost(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet_view","arguments":{}}}`)
	text := toolText(t, view)
	if !bytes.Contains([]byte(text), []byte(`"nodes"`)) {
		t.Fatalf("view: %s", text)
	}

	bad := mcpPost(t, h, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fleet_command","arguments":{"cmd":"nope"}}}`)
	if !bytes.Contains([]byte(toolText(t, bad)), []byte("unknown_cmd")) {
		t.Fatalf("bad cmd: %s", toolText(t, bad))
	}
}

func mcpPost(t *testing.T, h http.Handler, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusAccepted {
		return nil
	}
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func toolText(t *testing.T, res map[string]any) string {
	t.Helper()
	result, _ := res["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content: %v", res)
	}
	item, _ := content[0].(map[string]any)
	text, _ := item["text"].(string)
	return text
}

func recBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
