package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ffmpegManager locates FFmpeg, and downloads a private copy when the system
// does not already provide one. All state is guarded by mu so the UI can poll
// download progress while the transfer runs in the background.
type ffmpegManager struct {
	log *appLogger

	mu          sync.Mutex
	path        string
	version     string
	source      string
	lastErr     string
	downloading bool
	progress    float64 // 0..1, or -1 when the size is unknown
	bytes       int64
	total       int64
	autoStarted bool

	client *http.Client
}

type ffmpegStatus struct {
	Available   bool    `json:"available"`
	Path        string  `json:"path"`
	Version     string  `json:"version"`
	Source      string  `json:"source"`
	Downloading bool    `json:"downloading"`
	Progress    float64 `json:"progress"`
	Bytes       int64   `json:"bytes"`
	Total       int64   `json:"total"`
	Error       string  `json:"error"`
}

var errFFmpegMissing = errors.New("未找到 FFmpeg")

func newFFmpegManager(log *appLogger) *ffmpegManager {
	return &ffmpegManager{
		log:    log,
		client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
	}
}

func ffmpegStatusOf(m *ffmpegManager) ffmpegStatus {
	if m == nil {
		return ffmpegStatus{Error: errFFmpegMissing.Error()}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return ffmpegStatus{
		Available:   m.path != "",
		Path:        m.path,
		Version:     m.version,
		Source:      m.source,
		Downloading: m.downloading,
		Progress:    m.progress,
		Bytes:       m.bytes,
		Total:       m.total,
		Error:       m.lastErr,
	}
}

func (m *ffmpegManager) Path() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.path
}

// Detect finds an existing FFmpeg binary. It is cheap enough to call at
// startup and after a download completes.
func (m *ffmpegManager) Detect() bool {
	cache, _ := cacheDir()
	candidates := []struct {
		path   string
		source string
	}{
		{os.Getenv("CHROMAFRAME_FFMPEG"), "环境变量 CHROMAFRAME_FFMPEG"},
		{chdirJoin(exeDir(), "ffmpeg.exe"), "程序目录"},
		{chdirJoin(exeDir(), "ffmpeg"), "程序目录"},
	}
	if cache != "" {
		candidates = append(candidates,
			struct {
				path   string
				source string
			}{filepath.Join(cache, "bin", ffmpegExeName()), "内置下载缓存"},
		)
	}
	if p, err := exec.LookPath(ffmpegExeName()); err == nil {
		candidates = append(candidates, struct {
			path   string
			source string
		}{p, "系统 PATH"})
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		candidates = append(candidates, struct {
			path   string
			source string
		}{p, "系统 PATH"})
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\ffmpeg\bin\ffmpeg.exe`,
			`C:\Program Files\ffmpeg\bin\ffmpeg.exe`,
			`C:\Program Files (x86)\ffmpeg\bin\ffmpeg.exe`,
		} {
			candidates = append(candidates, struct {
				path   string
				source string
			}{p, "常见安装位置"})
		}
	}

	seen := map[string]bool{}
	for _, c := range candidates {
		if c.path == "" || seen[c.path] || !fileExists(c.path) {
			continue
		}
		seen[c.path] = true
		version, err := ffmpegVersionOf(c.path)
		if err != nil {
			m.log.Printf("ffmpeg candidate rejected (%s): %v", c.path, err)
			continue
		}
		m.mu.Lock()
		wasMissing := m.path == ""
		m.path = c.path
		m.version = version
		m.source = c.source
		m.lastErr = ""
		m.mu.Unlock()
		if wasMissing {
			m.log.Printf("ffmpeg found: %s (%s)", c.path, version)
		}
		return true
	}

	m.mu.Lock()
	m.path = ""
	m.version = ""
	m.source = ""
	m.mu.Unlock()
	return false
}

// StartAutoDownload begins an automatic download when FFmpeg is missing. It is
// safe to call more than once.
func (m *ffmpegManager) StartAutoDownload(ctx context.Context) {
	if m.Path() != "" {
		return
	}
	m.mu.Lock()
	if m.autoStarted || m.downloading {
		m.mu.Unlock()
		return
	}
	m.autoStarted = true
	m.mu.Unlock()
	go func() {
		if err := m.Download(ctx); err != nil {
			m.log.Printf("automatic ffmpeg download failed: %v", err)
			m.mu.Lock()
			m.lastErr = err.Error()
			m.mu.Unlock()
		}
	}()
}

// Ensure returns a usable FFmpeg path, downloading it if necessary.
func (m *ffmpegManager) Ensure(ctx context.Context) (string, error) {
	if p := m.Path(); p != "" {
		return p, nil
	}
	if err := m.Download(ctx); err != nil {
		return "", err
	}
	if p := m.Path(); p != "" {
		return p, nil
	}
	return "", errFFmpegMissing
}

func (m *ffmpegManager) Download(ctx context.Context) error {
	m.mu.Lock()
	if m.downloading {
		m.mu.Unlock()
		return fmt.Errorf("FFmpeg 正在下载中")
	}
	if m.path != "" {
		m.mu.Unlock()
		return nil
	}
	m.downloading = true
	m.progress = 0
	m.bytes = 0
	m.total = 0
	m.lastErr = ""
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.downloading = false
		m.mu.Unlock()
	}()

	if runtime.GOOS != "windows" {
		err := fmt.Errorf("系统未安装 FFmpeg。请使用包管理器安装（例如 apt install ffmpeg / brew install ffmpeg），或设置 CHROMAFRAME_FFMPEG 指向可执行文件")
		m.setErr(err)
		return err
	}

	cache, err := cacheDir()
	if err != nil {
		m.setErr(err)
		return err
	}
	downloadDir := filepath.Join(cache, "downloads")
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		m.setErr(err)
		return err
	}
	zipPath := filepath.Join(downloadDir, "ffmpeg-"+randomHex(4)+".zip")
	defer os.Remove(zipPath)

	urls := []string{
		os.Getenv("CHROMAFRAME_FFMPEG_URL"),
		"https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip",
		"https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip",
	}
	var lastErr error
	for _, u := range urls {
		if strings.TrimSpace(u) == "" {
			continue
		}
		m.log.Printf("downloading ffmpeg from %s", u)
		if err := m.downloadTo(ctx, u, zipPath); err != nil {
			lastErr = err
			m.log.Printf("ffmpeg download source failed: %v", err)
			continue
		}
		if err := m.extractFFmpeg(zipPath); err != nil {
			lastErr = err
			m.log.Printf("ffmpeg extraction failed: %v", err)
			continue
		}
		if m.Detect() {
			m.log.Printf("ffmpeg installed: %s", m.Path())
			return nil
		}
		lastErr = fmt.Errorf("下载完成，但无法执行解压出的 FFmpeg")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的 FFmpeg 下载源")
	}
	m.setErr(lastErr)
	return lastErr
}

func (m *ffmpegManager) setErr(err error) {
	m.mu.Lock()
	m.lastErr = err.Error()
	m.progress = 0
	m.mu.Unlock()
}

func (m *ffmpegManager) downloadTo(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", appName+"/"+appVersion)
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败：HTTP %s", resp.Status)
	}
	m.mu.Lock()
	m.total = resp.ContentLength
	m.bytes = 0
	if resp.ContentLength > 0 {
		m.progress = 0
	} else {
		m.progress = -1
	}
	m.mu.Unlock()

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, 256*1024)
	lastReport := time.Time{}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			m.mu.Lock()
			m.bytes += int64(n)
			if m.total > 0 {
				m.progress = float64(m.bytes) / float64(m.total)
				if m.progress > 1 {
					m.progress = 1
				}
			}
			m.mu.Unlock()
			if time.Since(lastReport) > 250*time.Millisecond {
				lastReport = time.Now()
				m.log.Printf("ffmpeg download: %s / %s", humanBytes(m.bytesSnapshot()), humanBytes(m.totalSnapshot()))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return nil
}

func (m *ffmpegManager) bytesSnapshot() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bytes
}

func (m *ffmpegManager) totalSnapshot() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

func (m *ffmpegManager) extractFFmpeg(zipPath string) error {
	cache, err := cacheDir()
	if err != nil {
		return err
	}
	binDir := filepath.Join(cache, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("打开 FFmpeg 压缩包失败：%w", err)
	}
	defer zr.Close()

	var chosen *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := strings.ToLower(filepath.ToSlash(f.Name))
		if strings.HasSuffix(name, "/bin/ffmpeg.exe") || name == "ffmpeg.exe" {
			chosen = f
			break
		}
		if chosen == nil && strings.HasSuffix(name, "/ffmpeg.exe") {
			chosen = f
		}
	}
	if chosen == nil {
		return fmt.Errorf("压缩包中没有找到 ffmpeg.exe")
	}
	rc, err := chosen.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	final := filepath.Join(binDir, ffmpegExeName())
	tmp := final + ".tmp-" + randomHex(3)
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.progress = -1
	m.mu.Unlock()
	const maxBytes = 512 << 20
	if _, err := io.Copy(out, io.LimitReader(rc, maxBytes)); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Remove(final)
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return err
	}
	m.mu.Lock()
	m.progress = 1
	m.mu.Unlock()
	return nil
}

func ffmpegVersionOf(path string) (string, error) {
	out, err := runWithTimeout(8*time.Second, path, "-hide_banner", "-version")
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, trimTo(string(out), 200))
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		return "", fmt.Errorf("没有输出版本信息")
	}
	return line, nil
}

func ffmpegExeName() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(p)
}

func chdirJoin(dir, name string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, name)
}
