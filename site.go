package main

import (
	"crypto/subtle"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// siteServer serves the selected HTML file and its sibling assets on a second
// loopback port. Serving the page through HTTP (instead of file://) makes
// relative URLs, ES modules, workers and CDN requests behave exactly like a
// normal website, which is required for reliable Chromium rendering.
type siteServer struct {
	log   *appLogger
	token string
	ln    net.Listener
	srv   *http.Server
	port  int

	mu    sync.RWMutex
	root  string
	entry string
	rev   int
}

var cspMetaRe = regexp.MustCompile(`(?is)<meta[^>]*http-equiv\s*=\s*["']?\s*content-security-policy[^>]*>`)
var headOpenRe = regexp.MustCompile(`(?is)<head(?:\s[^>]*)?>`)
var htmlOpenRe = regexp.MustCompile(`(?is)<html(?:\s[^>]*)?>`)

func newSiteServer(token string, log *appLogger) (*siteServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("无法创建页面服务端口: %w", err)
	}
	s := &siteServer{
		log:   log,
		token: token,
		ln:    ln,
		port:  ln.Addr().(*net.TCPAddr).Port,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/__cf/controller.js", s.handleController)
	mux.HandleFunc("/__cf/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", s.handlePage)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("site server stopped: %v", err)
		}
	}()
	return s, nil
}

func (s *siteServer) Close() {
	if s == nil || s.srv == nil {
		return
	}
	_ = s.srv.Close()
}

func (s *siteServer) SetHTML(p string) (string, error) {
	p = fixWindowsPath(p)
	if p == "" {
		return "", fmt.Errorf("请选择一个 HTML 文件")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("无法读取 HTML 文件: %w", err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("这是一个文件夹，请选择 HTML 文件")
	}
	ext := strings.ToLower(filepath.Ext(abs))
	if ext != ".html" && ext != ".htm" {
		return "", fmt.Errorf("只支持 .html / .htm 文件")
	}
	s.mu.Lock()
	s.root = filepath.Dir(abs)
	s.entry = filepath.Base(abs)
	s.rev++
	s.mu.Unlock()
	return filepath.Base(abs), nil
}

func (s *siteServer) current() (root, entry string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.root, s.entry, s.root != "" && s.entry != ""
}

func (s *siteServer) URL(headless bool, t float64) string {
	q := url.Values{}
	q.Set("__cf", s.token)
	q.Set("__cf_t", strconv.FormatFloat(t, 'f', 3, 64))
	if headless {
		q.Set("__cf_headless", "1")
	} else {
		q.Set("__cf_headless", "0")
	}
	return fmt.Sprintf("http://127.0.0.1:%d/?%s", s.port, q.Encode())
}

func (s *siteServer) tokenOK(r *http.Request) bool {
	got := r.URL.Query().Get("__cf")
	want := s.token
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *siteServer) handleController(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(controllerJS)
}

func (s *siteServer) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root, entry, ok := s.current()
	if !ok {
		http.Error(w, "尚未选择 HTML 文件", http.StatusNotFound)
		return
	}

	if r.URL.Path == "/" || r.URL.Path == "/"+entry {
		if !s.tokenOK(r) {
			http.Error(w, "无效的页面令牌", http.StatusForbidden)
			return
		}
		s.serveEntry(w, r, root, entry)
		return
	}
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	if strings.Contains(clean, "..") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	full := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	if !withinRoot(root, full) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	st, err := os.Stat(full)
	if err == nil && st.IsDir() {
		idx := filepath.Join(full, "index.html")
		if fileExists(idx) {
			full = idx
		}
	}
	http.ServeFile(w, r, full)
}

func (s *siteServer) serveEntry(w http.ResponseWriter, r *http.Request, root, entry string) {
	full := filepath.Join(root, entry)
	data, err := os.ReadFile(full)
	if err != nil {
		http.Error(w, "无法读取 HTML 文件: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := injectController(data)
	if mime.TypeByExtension(strings.ToLower(filepath.Ext(entry))) == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(out)
}

// injectController removes any CSP meta tag that would block the injected
// script and inserts the controller before the page's own scripts.
func injectController(html []byte) []byte {
	s := string(html)
	s = cspMetaRe.ReplaceAllString(s, "")
	payload := `<script src="/__cf/controller.js?v=` + appVersion + `"></script>` +
		`<style id="__cf-reset">html{scrollbar-width:none!important}html::-webkit-scrollbar,body::-webkit-scrollbar{display:none!important;width:0!important;height:0!important}</style>`

	if loc := headOpenRe.FindStringIndex(s); loc != nil {
		return []byte(s[:loc[1]] + payload + s[loc[1]:])
	}
	if loc := htmlOpenRe.FindStringIndex(s); loc != nil {
		return []byte(s[:loc[1]] + "<head>" + payload + "</head>" + s[loc[1]:])
	}
	return []byte(payload + s)
}

func withinRoot(root, full string) bool {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}
