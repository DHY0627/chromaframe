package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// RenderOptions is one render job. Zero Duration/FPS/Width/Height fall back to
// defaults, which keeps the CLI pleasant to use.
type RenderOptions struct {
	HTMLPath string
	Output   string
	Width    int
	Height   int
	Duration float64
	FPS      int
}

type RenderSnapshot struct {
	Running     bool    `json:"running"`
	Progress    float64 `json:"progress"`
	Frame       int     `json:"frame"`
	TotalFrames int     `json:"totalFrames"`
	Stage       string  `json:"stage"`
	Error       string  `json:"error"`
	Output      string  `json:"output"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	FPS         int     `json:"fps"`
	Duration    float64 `json:"duration"`
	Elapsed     float64 `json:"elapsed"`
	ETA         float64 `json:"eta"`
	Speed       float64 `json:"speed"`
	FrameRev    int     `json:"frameRev"`
	StartedAt   int64   `json:"startedAt"`
}

type renderer struct {
	log  *appLogger
	ff   *ffmpegManager
	site *siteServer
	ctx  context.Context

	mu      sync.Mutex
	state   RenderSnapshot
	latest  []byte
	cancel  context.CancelFunc
	running bool

	onProgress func(RenderSnapshot)
}

func newRenderer(ctx context.Context, log *appLogger, ff *ffmpegManager, site *siteServer) *renderer {
	return &renderer{ctx: ctx, log: log, ff: ff, site: site}
}

func (r *renderer) Snapshot() RenderSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *renderer) LatestFrame() ([]byte, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latest == nil {
		return nil, 0
	}
	out := make([]byte, len(r.latest))
	copy(out, r.latest)
	return out, r.state.FrameRev
}

func (r *renderer) Cancel() bool {
	r.mu.Lock()
	cancel := r.cancel
	running := r.running
	r.mu.Unlock()
	if running && cancel != nil {
		cancel()
		return true
	}
	return false
}

func (r *renderer) Start(opts RenderOptions) (RenderSnapshot, error) {
	norm, err := normalizeRenderOptions(opts)
	if err != nil {
		return r.Snapshot(), err
	}
	if r.ff.Path() == "" {
		st := ffmpegStatusOf(r.ff)
		if st.Downloading {
			return r.Snapshot(), fmt.Errorf("FFmpeg 正在下载，请等待下载完成后再开始渲染")
		}
		return r.Snapshot(), fmt.Errorf("未找到 FFmpeg，请点击左侧的下载按钮，或安装 FFmpeg 后重试")
	}
	chrome, err := validateChromium()
	if err != nil {
		return r.Snapshot(), err
	}
	_ = chrome

	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return r.Snapshot(), fmt.Errorf("已有渲染任务正在进行")
	}
	runCtx, cancel := context.WithCancel(r.ctx)
	r.cancel = cancel
	r.running = true
	r.latest = nil
	r.state = RenderSnapshot{
		Running:     true,
		Stage:       "准备渲染",
		Output:      norm.Output,
		Width:       norm.Width,
		Height:      norm.Height,
		FPS:         norm.FPS,
		Duration:    norm.Duration,
		StartedAt:   time.Now().UnixMilli(),
		TotalFrames: int(math.Round(norm.Duration * float64(norm.FPS))),
	}
	snap := r.state
	r.mu.Unlock()

	go func() {
		err := r.run(runCtx, norm)
		cancel()
		r.mu.Lock()
		r.running = false
		r.cancel = nil
		r.state.Running = false
		r.state.FrameRev++
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(runCtx.Err(), context.Canceled) {
				r.state.Stage = "已取消"
				r.state.Error = "渲染已取消"
			} else {
				r.state.Stage = "渲染失败"
				r.state.Error = err.Error()
				r.log.Printf("render failed: %v", err)
			}
			r.state.Progress = 0
		} else {
			r.state.Stage = "渲染完成"
			r.state.Progress = 1
			r.state.Frame = r.state.TotalFrames
			r.state.ETA = 0
		}
		r.state.Elapsed = float64(time.Now().UnixMilli()-r.state.StartedAt) / 1000
		snap := r.state
		r.mu.Unlock()
		r.notify(snap)
	}()
	return snap, nil
}

func (r *renderer) RunSync(ctx context.Context, opts RenderOptions) error {
	norm, err := normalizeRenderOptions(opts)
	if err != nil {
		return err
	}
	if r.ff.Path() == "" {
		if _, err := r.ff.Ensure(ctx); err != nil {
			return err
		}
	}
	return r.run(ctx, norm)
}

func (r *renderer) notify(snap RenderSnapshot) {
	if r.onProgress != nil {
		r.onProgress(snap)
	}
}

func (r *renderer) update(fn func(*RenderSnapshot)) {
	r.mu.Lock()
	fn(&r.state)
	snap := r.state
	r.mu.Unlock()
	r.notify(snap)
}

func (r *renderer) run(ctx context.Context, opts RenderOptions) (err error) {
	started := time.Now()

	// 1. Serve the page.
	if _, err := r.site.SetHTML(opts.HTMLPath); err != nil {
		return err
	}
	chrome, err := validateChromium()
	if err != nil {
		return err
	}
	r.update(func(s *RenderSnapshot) {
		s.Running = true
		s.Output = opts.Output
		s.Width = opts.Width
		s.Height = opts.Height
		s.FPS = opts.FPS
		s.Duration = opts.Duration
		s.StartedAt = started.UnixMilli()
		s.Stage = "启动 " + chrome.displayName()
	})

	// 2. Start a private headless Chromium.
	profileDir, err := os.MkdirTemp("", "chromaframe-profile-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profileDir)

	optsList := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chrome.Path),
		chromedp.Flag("headless", "new"),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
		chromedp.Flag("enable-unsafe-swiftshader", true),
		chromedp.Flag("force-color-profile", "srgb"),
		chromedp.Flag("force-device-scale-factor", "1"),
		chromedp.Flag("disable-background-networking", false),
		chromedp.Flag("disable-features", "Translate,BlinkGenPropertyTrees,CalculateNativeWinOcclusion"),
		chromedp.WindowSize(opts.Width, opts.Height),
		chromedp.UserDataDir(profileDir),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, optsList...)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	tracker := &netTracker{last: time.Now(), pending: map[network.RequestID]string{}}
	chromedp.ListenTarget(browserCtx, func(ev any) {
		tracker.handle(ev)
	})

	viewURL := r.site.URL(true, 0)
	r.log.Printf("render %dx%d %dfps %.2fs -> %s", opts.Width, opts.Height, opts.FPS, opts.Duration, opts.Output)

	if err := chromedp.Run(browserCtx,
		network.Enable(),
		emulation.SetDeviceMetricsOverride(int64(opts.Width), int64(opts.Height), 1, false),
		emulation.SetFocusEmulationEnabled(true),
		chromedp.Navigate(viewURL),
	); err != nil {
		return fmt.Errorf("无法打开 Chromium 渲染页面：%w", err)
	}

	// 3. Wait until the page and every CDN resource has settled.
	r.update(func(s *RenderSnapshot) {
		s.Stage = "等待页面与 CDN 资源加载"
	})
	if err := r.waitReady(browserCtx, tracker, 90*time.Second); err != nil {
		return err
	}

	// 4. Anchor the virtual clock at 0 and capture the initial state.
	r.update(func(s *RenderSnapshot) { s.Stage = "校准时间轴" })
	if err := r.setTime(browserCtx, 0); err != nil {
		return fmt.Errorf("初始化时间轴失败：%w", err)
	}
	_ = r.waitReady(browserCtx, tracker, 15*time.Second)
	if err := r.setTime(browserCtx, 0); err != nil {
		return fmt.Errorf("初始化时间轴失败：%w", err)
	}

	// 5. Start FFmpeg. JPEG frames are streamed over stdin, so no temporary
	//    frame files ever touch the disk.
	tmpOut := opts.Output + ".part-" + randomHex(4) + ".mp4"
	defer os.Remove(tmpOut)
	ffmpegPath := r.ff.Path()
	args := []string{
		"-hide_banner", "-y",
		"-loglevel", "error",
		"-f", "image2pipe",
		"-framerate", strconv.Itoa(opts.FPS),
		"-i", "pipe:0",
		"-an",
		"-vf", "scale=in_range=full:out_range=tv",
		"-c:v", "libx264",
		"-preset", "medium",
		"-crf", "18",
		"-pix_fmt", "yuv420p",
		"-colorspace", "bt709",
		"-color_primaries", "bt709",
		"-color_trc", "bt709",
		"-movflags", "+faststart",
		tmpOut,
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	configureChild(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stderr := &tailBuffer{max: 16 << 10}
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法启动 FFmpeg：%w", err)
	}
	ffmpegDone := false
	defer func() {
		if !ffmpegDone {
			_ = stdin.Close()
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	total := int(math.Round(opts.Duration * float64(opts.FPS)))
	if total < 1 {
		total = 1
	}
	r.update(func(s *RenderSnapshot) {
		s.TotalFrames = total
		s.Stage = "渲染帧"
	})

	frameDur := 1.0 / float64(opts.FPS)
	for i := 0; i < total; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		t := float64(i) * frameDur
		if i == total-1 {
			t = math.Max(0, opts.Duration-frameDur)
		}
		if err := r.setTime(browserCtx, t); err != nil {
			return fmt.Errorf("第 %d 帧设置时间失败：%w", i+1, err)
		}
		var jpg []byte
		err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(c context.Context) error {
			var shotErr error
			jpg, shotErr = page.CaptureScreenshot().
				WithFormat(page.CaptureScreenshotFormatJpeg).
				WithQuality(92).
				Do(c)
			return shotErr
		}))
		if err != nil {
			return fmt.Errorf("第 %d 帧截图失败：%w", i+1, err)
		}
		if len(jpg) == 0 {
			return fmt.Errorf("第 %d 帧截图结果为空", i+1)
		}
		if _, err := stdin.Write(jpg); err != nil {
			return fmt.Errorf("写入 FFmpeg 失败：%w", err)
		}

		elapsed := time.Since(started).Seconds()
		progress := float64(i+1) / float64(total)
		eta := 0.0
		if progress > 0.01 {
			eta = elapsed / progress * (1 - progress)
		}
		r.mu.Lock()
		r.latest = jpg
		r.state.Frame = i + 1
		r.state.TotalFrames = total
		r.state.Progress = progress
		r.state.Elapsed = elapsed
		r.state.ETA = eta
		r.state.Speed = float64(i+1) / math.Max(elapsed, 0.001)
		r.state.FrameRev++
		snap := r.state
		r.mu.Unlock()
		if i%3 == 0 || i == total-1 {
			r.notify(snap)
		}
	}

	r.update(func(s *RenderSnapshot) { s.Stage = "编码收尾" })
	if err := stdin.Close(); err != nil {
		r.log.Printf("closing ffmpeg stdin: %v", err)
	}
	waitErr := cmd.Wait()
	ffmpegDone = true
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return fmt.Errorf("FFmpeg 编码失败：%s", trimTo(msg, 1200))
	}

	st, err := os.Stat(tmpOut)
	if err != nil || st.Size() == 0 {
		return fmt.Errorf("FFmpeg 未生成有效的 MP4 文件")
	}
	_ = os.Remove(opts.Output)
	if err := os.Rename(tmpOut, opts.Output); err != nil {
		return fmt.Errorf("无法写入导出文件：%w", err)
	}
	r.log.Printf("render done: %s (%s, %.1fs)", opts.Output, humanBytes(st.Size()), time.Since(started).Seconds())
	return nil
}

func (r *renderer) setTime(ctx context.Context, seconds float64) error {
	expr := fmt.Sprintf(`(function(){
  if (!window.__cf || !window.__cf.setTime) return Promise.resolve(0);
  return window.__cf.setTime(%s);
})()`, strconv.FormatFloat(seconds, 'f', 6, 64))
	var out float64
	if err := evalPromise(ctx, expr, &out); err != nil {
		return err
	}
	return nil
}

// evalPromise evaluates an expression and waits for its promise to resolve.
// chromedp's Evaluate does not set awaitPromise, and the controller API is
// entirely promise based.
func evalPromise(ctx context.Context, expression string, out any) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		res, exp, err := runtime.Evaluate(expression).
			WithAwaitPromise(true).
			WithReturnByValue(true).
			Do(c)
		if err != nil {
			return err
		}
		if exp != nil {
			if exp.Text != "" {
				return fmt.Errorf("页面脚本异常：%s", exp.Text)
			}
			return errors.New("页面脚本异常")
		}
		if out == nil || res == nil || res.Value == nil {
			return nil
		}
		return json.Unmarshal(res.Value, out)
	}))
}

func (r *renderer) waitReady(ctx context.Context, tracker *netTracker, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	quiet := 900 * time.Millisecond
	if timeout <= 20*time.Second {
		quiet = 350 * time.Millisecond
	}
	lastLog := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			pending := tracker.pendingList()
			if len(pending) > 0 {
				return fmt.Errorf("资源加载超时（%s），仍有 %d 个请求未完成：%s", timeout, len(pending), strings.Join(pending[:minInt(len(pending), 5)], "\n"))
			}
			return fmt.Errorf("页面加载超时（%s）", timeout)
		}
		var ready string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`document.readyState`, &ready))
		imagesOK, _ := r.imagesReady(ctx)
		fontsOK, _ := r.fontsReady(ctx)
		pending, last := tracker.snapshot()
		if time.Since(lastLog) > 2*time.Second {
			lastLog = time.Now()
			r.log.Printf("waitReady ready=%q pending=%d images=%v fonts=%v idle=%s", ready, pending, imagesOK, fontsOK, time.Since(last).Round(time.Millisecond))
		}
		if ready == "complete" && pending == 0 && imagesOK && fontsOK && time.Since(last) >= quiet {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(120 * time.Millisecond):
		}
	}
}

func (r *renderer) imagesReady(ctx context.Context) (bool, error) {
	var ok bool
	expr := `(function(){
  var imgs = Array.prototype.slice.call(document.images).filter(function(i){ return !i.complete; });
  if (!imgs.length) return Promise.resolve(true);
  return Promise.race([
    Promise.all(imgs.map(function(img){ return new Promise(function(res){
      img.addEventListener('load', res, {once:true});
      img.addEventListener('error', res, {once:true});
    }); })).then(function(){ return true; }),
    new Promise(function(res){ setTimeout(function(){ res(false); }, 5000); })
  ]);
})()`
	if err := evalPromise(ctx, expr, &ok); err != nil {
		return false, err
	}
	return ok, nil
}

func (r *renderer) fontsReady(ctx context.Context) (bool, error) {
	var ok bool
	expr := `(function(){
  if (!document.fonts || !document.fonts.ready) return Promise.resolve(true);
  return Promise.race([
    document.fonts.ready.then(function(){ return true; }),
    new Promise(function(res){ setTimeout(function(){ res(false); }, 5000); })
  ]);
})()`
	if err := evalPromise(ctx, expr, &ok); err != nil {
		return false, err
	}
	return ok, nil
}

// netTracker follows in-flight network requests so a frame is never captured
// while a CDN script, font, image or stylesheet is still loading.
type netTracker struct {
	mu      sync.Mutex
	pending map[network.RequestID]string
	last    time.Time
	seen    int
}

func (t *netTracker) handle(ev any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch e := ev.(type) {
	case *network.EventRequestWillBeSent:
		if e.Request == nil {
			return
		}
		u := e.Request.URL
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return
		}
		t.pending[e.RequestID] = u
		t.last = time.Now()
		t.seen++
	case *network.EventLoadingFinished:
		delete(t.pending, e.RequestID)
		t.last = time.Now()
	case *network.EventLoadingFailed:
		delete(t.pending, e.RequestID)
		t.last = time.Now()
	}
}

func (t *netTracker) snapshot() (int, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending), t.last
}

func (t *netTracker) pendingList() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.pending))
	for _, u := range t.pending {
		out = append(out, u)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func normalizeRenderOptions(opts RenderOptions) (RenderOptions, error) {
	p := fixWindowsPath(opts.HTMLPath)
	if p == "" {
		return opts, fmt.Errorf("请选择 HTML 文件")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return opts, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return opts, fmt.Errorf("无法读取 HTML 文件: %w", err)
	}
	if st.IsDir() {
		return opts, fmt.Errorf("请选择 HTML 文件而不是文件夹")
	}
	ext := strings.ToLower(filepath.Ext(abs))
	if ext != ".html" && ext != ".htm" {
		return opts, fmt.Errorf("只支持 .html / .htm 文件")
	}
	opts.HTMLPath = abs

	if opts.Width == 0 {
		opts.Width = 1920
	}
	if opts.Height == 0 {
		opts.Height = 1080
	}
	if opts.FPS == 0 {
		opts.FPS = 30
	}
	if opts.Duration == 0 {
		opts.Duration = 10
	}
	if opts.Width < 64 || opts.Width > 7680 {
		return opts, fmt.Errorf("宽度需在 64 到 7680 之间")
	}
	if opts.Height < 64 || opts.Height > 7680 {
		return opts, fmt.Errorf("高度需在 64 到 7680 之间")
	}
	if opts.Width%2 != 0 || opts.Height%2 != 0 {
		return opts, fmt.Errorf("H.264 需要偶数分辨率，请把宽高调整为偶数")
	}
	if opts.FPS < 1 || opts.FPS > 120 {
		return opts, fmt.Errorf("帧率需在 1 到 120 之间")
	}
	if opts.Duration < 0.1 || opts.Duration > 3600 {
		return opts, fmt.Errorf("时长需在 0.1 到 3600 秒之间")
	}

	out := fixWindowsPath(opts.Output)
	if out == "" {
		out = defaultOutputFor(abs)
	}
	if !strings.EqualFold(filepath.Ext(out), ".mp4") {
		out += ".mp4"
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return opts, err
	}
	if outAbs == abs {
		return opts, fmt.Errorf("导出文件不能与 HTML 文件相同")
	}
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return opts, fmt.Errorf("无法创建导出目录: %w", err)
	}
	opts.Output = outAbs
	return opts, nil
}

func defaultOutputFor(htmlPath string) string {
	base := strings.TrimSuffix(filepath.Base(htmlPath), filepath.Ext(htmlPath))
	if strings.TrimSpace(base) == "" {
		base = "chromaframe"
	}
	return filepath.Join(filepath.Dir(htmlPath), base+".mp4")
}

func runCLIRender(log *appLogger, opts RenderOptions) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ff := newFFmpegManager(log)
	if !ff.Detect() {
		fmt.Println("未找到 FFmpeg，开始自动下载…")
		if _, err := ff.Ensure(ctx); err != nil {
			return err
		}
	}
	chrome, err := validateChromium()
	if err != nil {
		return err
	}
	if chrome.Version != "" {
		fmt.Printf("Chromium: %s (%s)\n", chrome.Path, chrome.Version)
	} else {
		fmt.Printf("Chromium: %s (%s)\n", chrome.Path, chrome.displayName())
	}
	fmt.Printf("FFmpeg:   %s\n", ff.Path())

	token := randomHex(16)
	site, err := newSiteServer(token, log)
	if err != nil {
		return err
	}
	defer site.Close()

	r := newRenderer(ctx, log, ff, site)
	lastPct := -1
	r.onProgress = func(s RenderSnapshot) {
		if s.Error != "" {
			fmt.Printf("\n%s: %s\n", s.Stage, s.Error)
			return
		}
		pct := int(s.Progress * 100)
		if pct != lastPct && (pct%5 == 0 || s.Frame == s.TotalFrames) {
			lastPct = pct
			fmt.Printf("[%3d%%] %d/%d 帧  %.1f fps  %s\n", pct, s.Frame, s.TotalFrames, s.Speed, s.Stage)
		}
	}
	if err := r.RunSync(ctx, opts); err != nil {
		return err
	}
	snap := r.Snapshot()
	fmt.Printf("完成: %s (%.1f 秒)\n", snap.Output, snap.Elapsed)
	return nil
}
