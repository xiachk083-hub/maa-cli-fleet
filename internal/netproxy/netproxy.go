// Package netproxy —— 读取 Windows 系统代理（WinINET），供出网请求使用。
//
// 背景：很多国内机器只有走系统代理才能访问 GitHub；Go 默认直连会超时，
// 而 PowerShell 会自动用系统代理——这就是"PS 能下 59MB/s、Go 却连不上"的原因。
package netproxy

import (
	"net/http"
	"net/url"
	"os/exec"
	"strings"
)

// Func 返回 http.Transport 用的 Proxy 函数：公网走系统代理，内网/本机直连。
func Func() func(*http.Request) (*url.URL, error) {
	server := systemProxyServer()
	if server == "" {
		return nil
	}
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	u, err := url.Parse(server)
	if err != nil {
		return nil
	}
	return func(req *http.Request) (*url.URL, error) {
		h := req.URL.Hostname()
		// 本机 / 内网 / Tailscale 一律直连
		if h == "localhost" || h == "127.0.0.1" || strings.HasPrefix(h, "192.168.") ||
			strings.HasPrefix(h, "10.") || strings.HasPrefix(h, "100.") || strings.HasPrefix(h, "172.") {
			return nil, nil
		}
		return u, nil
	}
}

// NewClient 建一个带系统代理的 http.Client（Transport 从 DefaultTransport 克隆）。
func NewClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if f := Func(); f != nil {
		t.Proxy = f
	}
	return &http.Client{Transport: t}
}

func systemProxyServer() string {
	enable := regQuery("ProxyEnable")
	if !strings.Contains(enable, "1") {
		return ""
	}
	return regQuery("ProxyServer")
}

func regQuery(name string) string {
	out, err := exec.Command("reg", "query",
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`, "/v", name).Output()
	if err != nil {
		return ""
	}
	// 形如：  ProxyServer    REG_SZ    http://127.0.0.1:7890
	fields := strings.Fields(string(out))
	if len(fields) >= 3 {
		return strings.TrimSpace(fields[len(fields)-1])
	}
	return ""
}
