// Package setup —— 机端自举：自己下载 maa-cli，自己装 MaaCore + 资源（不依赖任何旧安装）。
//
//	fleet setup [-root <项目>] [-data <MAA_DATA_DIR>] [-bin <maa.exe 目录>] [-force] [-channel stable]
//
// 步骤：
//  1. <bin>\maa.exe 缺失（或 -force）→ 从 maa-cli 最新 Release 下 x86_64-pc-windows-msvc.zip 解出
//  2. 跑 maa install（首次）/ maa update（已装过），全部写进项目目录（MAA_*_DIR 重定向）
//  3. 打印版本与目录
package setup

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const latestAPI = "https://api.github.com/repos/MaaAssistantArknights/maa-cli/releases/latest"

// Options 自举参数。
type Options struct {
	Root    string // 项目根
	BinDir  string // maa.exe 放哪（默认 <root>\bin）
	DataDir string // MAA_DATA_DIR（默认 <root>\data）
	Force   bool   // 强制重下 maa-cli
	Channel string // stable / beta / alpha
}

// Run 执行自举。
func Run(o Options) error {
	if o.Root == "" {
		return fmt.Errorf("需要 -root")
	}
	if o.BinDir == "" {
		o.BinDir = filepath.Join(o.Root, "bin")
	}
	if o.DataDir == "" {
		o.DataDir = filepath.Join(o.Root, "data")
	}
	if o.Channel == "" {
		o.Channel = "stable"
	}
	maaExe := filepath.Join(o.BinDir, "maa.exe")

	if o.Force || !exists(maaExe) {
		url, tag, err := latestMaaCliZip()
		if err != nil {
			return fmt.Errorf("查 maa-cli 最新版本失败：%w", err)
		}
		log.Printf("[setup] 下载 maa-cli %s → %s", tag, url)
		if err := downloadAndUnzipMaaCli(url, o.BinDir); err != nil {
			return fmt.Errorf("下载/解压 maa-cli 失败：%w", err)
		}
	}
	if !exists(maaExe) {
		return fmt.Errorf("没拿到 maa.exe：%s", maaExe)
	}

	env := append(os.Environ(),
		"MAA_CONFIG_DIR="+filepath.Join(o.Root, "config"),
		"MAA_DATA_DIR="+o.DataDir,
		"MAA_CACHE_DIR="+filepath.Join(o.DataDir, "cache"),
	)

	installed := exists(filepath.Join(o.DataDir, "resource")) || exists(filepath.Join(o.DataDir, "lib"))
	args := []string{"install", o.Channel}
	if installed {
		args = []string{"update"}
		log.Printf("[setup] 检测到已装过 → 走 maa update")
	} else {
		log.Printf("[setup] 首次安装 → 走 maa install %s（含镜像测速）", o.Channel)
	}
	cmd := exec.Command(maaExe, args...)
	cmd.Env = env
	cmd.Dir = filepath.Dir(maaExe)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("maa %s 失败：%w", strings.Join(args, " "), err)
	}

	// 版本核对
	for _, a := range [][]string{{"version"}, {"version", "core"}, {"version", "resource"}} {
		out, _ := exec.Command(maaExe, a...).CombinedOutput()
		log.Printf("[setup] maa %s → %s", strings.Join(a, " "), strings.TrimSpace(string(out)))
	}
	log.Printf("[setup] 完成：bin=%s data=%s", o.BinDir, o.DataDir)
	return nil
}

// latestMaaCliZip 查最新 Release 里的 windows x86_64 资产。
func latestMaaCliZip() (url, tag string, err error) {
	c := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("GET", latestAPI, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "maa-cli-fleet")
	resp, err := c.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", err
	}
	for _, a := range rel.Assets {
		if strings.Contains(a.Name, "x86_64-pc-windows-msvc.zip") && !strings.Contains(a.Name, "winget") {
			return a.URL, rel.TagName, nil
		}
	}
	return "", rel.TagName, fmt.Errorf("release %s 里没有 x86_64-pc-windows-msvc.zip", rel.TagName)
}

// downloadAndUnzipMaaCli 下载 zip 并解出 maa.exe 到 binDir。
func downloadAndUnzipMaaCli(url, binDir string) error {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	zipPath := filepath.Join(os.TempDir(), fmt.Sprintf("maa_cli_%d.zip", time.Now().Unix()))
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	f.Close()
	defer os.Remove(zipPath)

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	found := 0
	for _, zf := range zr.File {
		if strings.EqualFold(filepath.Base(zf.Name), "maa.exe") {
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			dst := filepath.Join(binDir, "maa.exe")
			out, err := os.Create(dst)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(out, rc)
			out.Close()
			rc.Close()
			if err != nil {
				return err
			}
			found++
			log.Printf("[setup] 解出 %s（%d 字节）", dst, zf.UncompressedSize64)
		}
	}
	if found == 0 {
		return fmt.Errorf("zip 里没有 maa.exe")
	}
	return nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
