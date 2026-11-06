package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// chromiumInfo describes a browser that can drive both the app window and the
// headless renderer. Chromaframe uses whatever Chrome/Chromium/Edge is already
// installed; it never bundles a browser.
type chromiumInfo struct {
	Available bool   `json:"available"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Source    string `json:"source"`
	Error     string `json:"error"`
}

var (
	chromiumOnce   sync.Once
	chromiumCache  chromiumInfo
	chromiumCached bool
)

func detectChromium() chromiumInfo {
	chromiumOnce.Do(func() {
		chromiumCache = locateChromium()
		chromiumCached = true
	})
	if !chromiumCached {
		return chromiumInfo{Error: "Chromium 检测未完成"}
	}
	return chromiumCache
}

func locateChromium() chromiumInfo {
	type candidate struct {
		path   string
		name   string
		source string
	}
	var list []candidate
	add := func(path, name, source string) {
		if strings.TrimSpace(path) != "" {
			list = append(list, candidate{path: path, name: name, source: source})
		}
	}

	add(os.Getenv("CHROMAFRAME_CHROME"), "Chromium (环境变量)", "环境变量 CHROMAFRAME_CHROME")

	switch runtime.GOOS {
	case "windows":
		var roots []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
			if v := os.Getenv(env); v != "" {
				roots = append(roots, v)
			}
		}
		for _, root := range roots {
			add(filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"), "Google Chrome", "默认安装位置")
			add(filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"), "Microsoft Edge", "默认安装位置")
			add(filepath.Join(root, "Chromium", "Application", "chrome.exe"), "Chromium", "默认安装位置")
		}
		add(`C:\Program Files\Google\Chrome\Application\chrome.exe`, "Google Chrome", "默认安装位置")
		add(`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`, "Google Chrome", "默认安装位置")
		add(`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`, "Microsoft Edge", "默认安装位置")
		add(`C:\Program Files\Microsoft\Edge\Application\msedge.exe`, "Microsoft Edge", "默认安装位置")
	case "darwin":
		add("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "Google Chrome", "默认安装位置")
		add("/Applications/Chromium.app/Contents/MacOS/Chromium", "Chromium", "默认安装位置")
		add("/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge", "Microsoft Edge", "默认安装位置")
	default:
		add("/usr/bin/google-chrome", "Google Chrome", "默认安装位置")
		add("/usr/bin/google-chrome-stable", "Google Chrome", "默认安装位置")
		add("/usr/bin/chromium", "Chromium", "默认安装位置")
		add("/usr/bin/chromium-browser", "Chromium", "默认安装位置")
		add("/usr/bin/microsoft-edge", "Microsoft Edge", "默认安装位置")
	}

	for _, name := range []string{"chrome.exe", "msedge.exe", "chromium.exe", "chrome", "chromium", "google-chrome", "microsoft-edge"} {
		if p, err := exec.LookPath(name); err == nil {
			list = append(list, candidate{p, name, "系统 PATH"})
		}
	}

	for _, c := range list {
		if !fileExists(c.path) {
			continue
		}
		info := chromiumInfo{
			Available: true,
			Path:      c.path,
			Name:      c.name,
			Source:    c.source,
			Version:   chromiumVersionOf(c.path),
		}
		return info
	}
	return chromiumInfo{Error: "未找到 Chrome / Chromium / Edge。请安装 Google Chrome 或 Microsoft Edge，或设置 CHROMAFRAME_CHROME 指向浏览器可执行文件。"}
}

func chromiumVersionOf(path string) string {
	// Do NOT run chrome.exe --version on Windows: Chrome treats the command as
	// "open my existing browser session" and pops a window. That is exactly the
	// behavior Chromaframe must avoid. The version string is cosmetic, so skip it.
	if runtime.GOOS == "windows" {
		return ""
	}
	out, err := runWithTimeout(8*time.Second, path, "--version")
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	return trimTo(line, 120)
}

func (c chromiumInfo) displayName() string {
	if c.Name != "" {
		return c.Name
	}
	return "Chromium"
}

func validateChromium() (chromiumInfo, error) {
	info := detectChromium()
	if !info.Available {
		if info.Error == "" {
			info.Error = "未找到 Chromium"
		}
		return info, fmt.Errorf("%s", info.Error)
	}
	return info, nil
}
