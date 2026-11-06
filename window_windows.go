//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	webview2 "github.com/jchv/go-webview2"
)

// RunNativeWindow hosts the Chromaframe UI inside an embedded WebView2 window.
// WebView2 is the Chromium engine that ships with Windows, so the app never
// launches the user's Chrome/Edge browser as a separate window.
func (a *App) RunNativeWindow() error {
	dataRoot, err := cacheDir()
	if err != nil {
		dataRoot = filepath.Join(os.TempDir(), "Chromaframe")
	}
	dataPath := filepath.Join(dataRoot, "webview")
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     a.verbose,
		AutoFocus: true,
		DataPath:  dataPath,
		WindowOptions: webview2.WindowOptions{
			Title:  appName + " — HTML → MP4",
			Width:  1480,
			Height: 960,
			Center: true,
		},
	})
	if w == nil {
		return errors.New("无法初始化内嵌 Chromium（WebView2）。请安装 Microsoft Edge WebView2 Runtime 后重试")
	}
	defer w.Destroy()

	w.Navigate(a.URL())
	go func() {
		<-a.ctx.Done()
		w.Terminate()
	}()
	w.Run()
	return nil
}
