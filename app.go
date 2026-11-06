package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// App owns the local UI server, the page server and the renderer.
type App struct {
	ctx     context.Context
	log     *appLogger
	verbose bool
	token   string
	logPath string

	site   *siteServer
	ffmpeg *ffmpegManager
	render *renderer
	chrome chromiumInfo

	mu       sync.Mutex
	ln       *listenHandle
	srv      *serverHandle
	port     int
	window   *exec.Cmd
	profile  string
	done     chan struct{}
	closeOne sync.Once
	closed   bool
}

func NewApp(ctx context.Context, log *appLogger, verbose bool) (*App, error) {
	token := randomHex(24)
	site, err := newSiteServer(token, log)
	if err != nil {
		return nil, err
	}
	ff := newFFmpegManager(log)
	if !ff.Detect() {
		log.Printf("ffmpeg not found; automatic download scheduled")
		ff.StartAutoDownload(ctx)
	}
	chrome := detectChromium()
	if chrome.Available {
		log.Printf("chromium: %s (%s)", chrome.Path, chrome.Version)
	} else {
		log.Printf("chromium not found: %s", chrome.Error)
	}

	a := &App{
		ctx:     ctx,
		log:     log,
		verbose: verbose,
		token:   token,
		site:    site,
		ffmpeg:  ff,
		chrome:  chrome,
		done:    make(chan struct{}),
	}
	a.render = newRenderer(ctx, log, ff, site)
	return a, nil
}

func (a *App) Done() <-chan struct{} { return a.done }

func (a *App) Token() string { return a.token }

func (a *App) ServerPort() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.port
}

func (a *App) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/?token=%s", a.ServerPort(), a.token)
}

func (a *App) OpenWindow() error {
	chrome, err := validateChromium()
	if err != nil {
		return err
	}
	profile, err := os.MkdirTemp("", "chromaframe-ui-")
	if err != nil {
		return fmt.Errorf("无法创建应用窗口配置目录: %w", err)
	}
	args := []string{
		"--app=" + a.URL(),
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-background-mode",
		"--disable-features=Translate,MediaRouter",
		"--window-size=1480,960",
		"--window-position=80,60",
	}
	cmd := exec.Command(chrome.Path, args...)
	configureChild(cmd)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(profile)
		return fmt.Errorf("启动 %s 失败: %w", chrome.displayName(), err)
	}
	a.mu.Lock()
	a.window = cmd
	a.profile = profile
	a.mu.Unlock()
	go func() {
		err := cmd.Wait()
		if err != nil {
			a.log.Printf("app window exited: %v", err)
		}
		a.signalDone()
	}()
	return nil
}

func (a *App) signalDone() {
	a.closeOne.Do(func() { close(a.done) })
}

func (a *App) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	srv := a.srv
	window := a.window
	profile := a.profile
	a.mu.Unlock()

	a.render.Cancel()
	if srv != nil {
		srv.Close()
	}
	if a.site != nil {
		a.site.Close()
	}
	if window != nil && window.Process != nil {
		_ = window.Process.Kill()
	}
	if profile != "" {
		_ = os.RemoveAll(profile)
	}
	a.log.Printf("closed")
}

// listenHandle/serverHandle keep app.go free of net/http details.
type listenHandle struct{}
type serverHandle struct {
	srv    interface{ Close() error }
	lnAddr string
}

func (s *serverHandle) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

func (a *App) LogPath() string {
	dir, err := cacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "logs", "chromaframe.log")
}

func (a *App) platformName() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	default:
		return "Linux"
	}
}

func (a *App) uptime() time.Duration { return 0 }
