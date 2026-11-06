package main

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const authCookie = "cf_token"

type uiState struct {
	Version       string         `json:"version"`
	Platform      string         `json:"platform"`
	LogPath       string         `json:"logPath"`
	FFmpeg        ffmpegStatus   `json:"ffmpeg"`
	Chromium      chromiumInfo   `json:"chromium"`
	Render        RenderSnapshot `json:"render"`
	HasHTML       bool           `json:"hasHtml"`
	HTMLPath      string         `json:"htmlPath"`
	HTMLName      string         `json:"htmlName"`
	DefaultOutput string         `json:"defaultOutput"`
	PreviewURL    string         `json:"previewUrl"`
	Token         string         `json:"token"`
}

func (a *App) StartServer(requestedPort int) error {
	addr := "127.0.0.1:0"
	if requestedPort > 0 {
		addr = fmt.Sprintf("127.0.0.1:%d", requestedPort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("无法监听本地端口: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/static/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(appJS)
	})
	mux.HandleFunc("/static/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(styleCSS)
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/state", a.auth(a.handleState))
	mux.HandleFunc("/api/html", a.auth(a.handleSetHTML))
	mux.HandleFunc("/api/render", a.auth(a.handleRender))
	mux.HandleFunc("/api/cancel", a.auth(a.handleCancel))
	mux.HandleFunc("/api/frame", a.auth(a.handleFrame))
	mux.HandleFunc("/api/dialog", a.auth(a.handleDialog))
	mux.HandleFunc("/api/open", a.auth(a.handleOpen))
	mux.HandleFunc("/api/ffmpeg/download", a.auth(a.handleFFmpegDownload))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	a.mu.Lock()
	a.port = port
	a.srv = &serverHandle{srv: srv, lnAddr: ln.Addr().String()}
	a.mu.Unlock()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			a.log.Printf("ui server stopped: %v", err)
			a.signalDone()
		}
	}()
	a.log.Printf("UI listening on http://127.0.0.1:%d/", port)
	return nil
}

func (a *App) authorized(r *http.Request) bool {
	if c, err := r.Cookie(authCookie); err == nil {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.token)) == 1 {
			return true
		}
	}
	if h := r.Header.Get("X-CF-Token"); h != "" {
		if subtle.ConstantTimeCompare([]byte(h), []byte(a.token)) == 1 {
			return true
		}
	}
	if q := r.URL.Query().Get("token"); q != "" {
		if subtle.ConstantTimeCompare([]byte(q), []byte(a.token)) == 1 {
			return true
		}
	}
	return false
}

func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			writeError(w, http.StatusUnauthorized, "未授权的请求，请从 Chromaframe 窗口打开界面")
			return
		}
		next(w, r)
	}
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if tok := r.URL.Query().Get("token"); tok != "" {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) != 1 {
			http.Error(w, "无效的访问令牌", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     authCookie,
			Value:    a.token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   24 * 3600,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !a.authorized(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Chromaframe</title>
<body style="background:#0b0c0e;color:#e8e6e1;font:15px/1.6 system-ui,sans-serif;padding:48px">
<h1 style="font-weight:600">Chromaframe 正在运行</h1>
<p>请通过 Chromaframe 应用窗口访问，或使用启动日志中带 token 的地址。</p>
</body>`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	root, entry, has := a.site.current()
	htmlPath := ""
	previewURL := ""
	if has {
		htmlPath = filepath.Join(root, entry)
		previewURL = a.site.URL(false, 0)
	}
	resp := uiState{
		Version:  appVersion,
		Platform: a.platformName(),
		LogPath:  a.LogPath(),
		FFmpeg:   ffmpegStatusOf(a.ffmpeg),
		Chromium: a.chrome,
		Render:   a.render.Snapshot(),
		HasHTML:  has,
		HTMLPath: htmlPath,
		HTMLName: entry,
		DefaultOutput: func() string {
			if !has {
				return ""
			}
			return defaultOutputFor(filepath.Join(root, entry))
		}(),
		PreviewURL: previewURL,
		Token:      a.token,
	}
	// Refresh Chromium once if it was missing at startup (the user may have
	// installed a browser while the app was open).
	if !resp.Chromium.Available {
		resp.Chromium = detectChromium()
	}
	writeJSON(w, http.StatusOK, resp)
}

type setHTMLRequest struct {
	Path string `json:"path"`
}

type setHTMLResponse struct {
	Path          string `json:"path"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	DefaultOutput string `json:"defaultOutput"`
}

func (a *App) handleSetHTML(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req setHTMLRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "请提供 HTML 文件路径")
		return
	}
	name, err := a.site.SetHTML(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	abs := fixWindowsPath(req.Path)
	if abs2, err := filepath.Abs(abs); err == nil {
		abs = abs2
	}
	writeJSON(w, http.StatusOK, setHTMLResponse{
		Path:          abs,
		Name:          name,
		URL:           a.site.URL(false, 0),
		DefaultOutput: defaultOutputFor(abs),
	})
}

type renderRequest struct {
	HTMLPath  string  `json:"htmlPath"`
	Output    string  `json:"output"`
	AudioPath string  `json:"audioPath"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Duration  float64 `json:"duration"`
	FPS       int     `json:"fps"`
}

func (a *App) handleRender(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req renderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	snap, err := a.render.Start(RenderOptions{
		HTMLPath:  req.HTMLPath,
		Output:    req.Output,
		AudioPath: req.AudioPath,
		Width:     req.Width,
		Height:    req.Height,
		Duration:  req.Duration,
		FPS:       req.FPS,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, snap)
}

func (a *App) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"canceled": a.render.Cancel()})
}

func (a *App) handleFrame(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	frame, rev := a.render.LatestFrame()
	if len(frame) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Rev", fmt.Sprintf("%d", rev))
	_, _ = w.Write(frame)
}

type dialogRequest struct {
	Kind string `json:"kind"`
}

func (a *App) handleDialog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req dialogRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	path, err := pickPath(req.Kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "canceled": path == ""})
}

type openRequest struct {
	Path string `json:"path"`
}

func (a *App) handleOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req openRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	p := fixWindowsPath(req.Path)
	if p == "" {
		writeError(w, http.StatusBadRequest, "请提供路径")
		return
	}
	if _, err := os.Stat(p); err != nil {
		writeError(w, http.StatusBadRequest, "路径不存在: %s", p)
		return
	}
	if err := openExternal(p); err != nil {
		writeError(w, http.StatusInternalServerError, "无法打开: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleFFmpegDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if a.ffmpeg.Path() != "" {
		writeJSON(w, http.StatusOK, map[string]any{"started": false, "available": true})
		return
	}
	a.ffmpeg.StartAutoDownload(a.ctx)
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}
