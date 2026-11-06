//go:build !windows

package main

// RunNativeWindow falls back to opening the UI in a Chromium app window on
// platforms without an embedded WebView2 host.
func (a *App) RunNativeWindow() error {
	if err := a.OpenWindow(); err != nil {
		return err
	}
	select {
	case <-a.ctx.Done():
	case <-a.Done():
	}
	return nil
}
