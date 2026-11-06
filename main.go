// Chromaframe turns a local HTML page into an MP4 by rendering it frame by
// frame in Chromium and encoding the frames with FFmpeg.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	appName    = "Chromaframe"
	appVersion = "1.0.0"
)

func main() {
	var (
		uiPort     = flag.Int("port", 0, "UI 服务端口，0 表示自动选择")
		noBrowser  = flag.Bool("no-browser", false, "只启动本地服务，不打开应用窗口（调试用）")
		verbose    = flag.Bool("verbose", false, "输出调试日志")
		showVer    = flag.Bool("version", false, "显示版本")
		renderMode = flag.Bool("render", false, "命令行渲染模式（无界面）")
		htmlPath   = flag.String("html", "", "render 模式：HTML 文件路径")
		outPath    = flag.String("out", "", "render 模式：输出 MP4 路径")
		audioPath  = flag.String("audio", "", "render 模式：可选音轨文件（MP3/WAV/M4A 等）")
		width      = flag.Int("width", 1920, "render 模式：宽度")
		height     = flag.Int("height", 1080, "render 模式：高度")
		duration   = flag.Float64("duration", 10, "render 模式：时长（秒）")
		fps        = flag.Int("fps", 30, "render 模式：帧率")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("%s %s (%s/%s)\n", appName, appVersion, runtime.GOOS, runtime.GOARCH)
		return
	}

	logger, logPath := newLogger(*verbose)
	defer logger.Close()

	if *renderMode {
		if *htmlPath == "" || *outPath == "" {
			fmt.Fprintln(os.Stderr, "render 模式需要 --html 与 --out")
			os.Exit(2)
		}
		if err := runCLIRender(logger, RenderOptions{
			HTMLPath:  *htmlPath,
			Output:    *outPath,
			AudioPath: *audioPath,
			Width:     *width,
			Height:    *height,
			Duration:  *duration,
			FPS:       *fps,
		}); err != nil {
			logger.Printf("render failed: %v", err)
			fmt.Fprintln(os.Stderr, "渲染失败:", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := NewApp(ctx, logger, *verbose)
	if err != nil {
		logger.Printf("startup failed: %v", err)
		fatalDialog(fmt.Sprintf("%s 启动失败：\n%v\n\n日志：%s", appName, err, logPath))
		os.Exit(1)
	}
	defer app.Close()

	if err := app.StartServer(*uiPort); err != nil {
		logger.Printf("server failed: %v", err)
		fatalDialog(fmt.Sprintf("%s 无法启动本地服务：\n%v\n\n日志：%s", appName, err, logPath))
		os.Exit(1)
	}
	logger.Printf("%s %s started; UI http://127.0.0.1:%d/", appName, appVersion, app.ServerPort())
	if *noBrowser {
		fmt.Printf("UI ready: http://127.0.0.1:%d/\n", app.ServerPort())
		fmt.Printf("token: %s\n", app.Token())
		select {
		case <-ctx.Done():
		case <-app.Done():
		}
		logger.Printf("shutting down")
		app.Close()
		return
	}

	// Host the UI in an embedded Chromium window (WebView2). No external
	// browser window is launched.
	if err := app.RunNativeWindow(); err != nil {
		logger.Printf("native window failed: %v", err)
		fatalDialog(fmt.Sprintf("%s 无法打开主窗口：\n%v\n\n日志：%s", appName, err, logPath))
		os.Exit(1)
	}
	logger.Printf("shutting down")
	app.Close()
}

// newLogger writes to stderr (dev console) and a rotating-ish daily log file
// beside the user's cache. The returned closer may be nil-safe.
func newLogger(verbose bool) (*appLogger, string) {
	dir, err := cacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	logDir := filepath.Join(dir, "logs")
	_ = os.MkdirAll(logDir, 0o755)
	logPath := filepath.Join(logDir, "chromaframe.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return &appLogger{Logger: log.New(os.Stderr, "", log.LstdFlags), console: os.Stderr}, logPath
	}
	var out io.Writer = f
	if verbose {
		out = io.MultiWriter(f, os.Stderr)
	}
	return &appLogger{Logger: log.New(out, "", log.LstdFlags), file: f, console: os.Stderr, verbose: verbose}, logPath
}

type appLogger struct {
	*log.Logger
	file    *os.File
	console io.Writer
	verbose bool
}

func (l *appLogger) Close() {
	if l == nil {
		return
	}
	if l.file != nil {
		_ = l.file.Close()
	}
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("等待文件超时: %s", path)
}
