//go:build windows

package main

import (
	"sync"
	"sync/atomic"

	"github.com/wailsapp/go-webview2/pkg/edge"
)

var webView2ObserverState = struct {
	sync.Once
	events  chan edge.ProcessFailedDiagnostic
	dropped atomic.Uint64
}{events: make(chan edge.ProcessFailedDiagnostic, 32)}

func installWebView2ProcessObserver(app *App) {
	if app == nil {
		return
	}
	nativeWebView2ObserverInstalled.Store(true)
	process := func(diagnostic edge.ProcessFailedDiagnostic) {
		// WebView2 process failures are observed locally; crash/metrics
		// reporting was removed.
		_ = diagnostic
	}
	webView2ObserverState.Do(func() {
		go func() {
			for diagnostic := range webView2ObserverState.events {
				process(diagnostic)
			}
		}()
		edge.SetProcessFailedObserver(func(diagnostic edge.ProcessFailedDiagnostic) {
			if diagnostic.Kind == edge.COREWEBVIEW2_PROCESS_FAILED_KIND_BROWSER_PROCESS_EXITED {
				// Wails exits immediately after its callback. Preserve the fatal
				// report and outcome synchronously before control returns to Wails.
				process(diagnostic)
				return
			}
			select {
			case webView2ObserverState.events <- diagnostic:
			default:
				// Keep the COM callback non-blocking and let the background consumer
				// expose queue pressure as an aggregate, content-free metric.
				webView2ObserverState.dropped.Add(1)
			}
		})
	})
}

func refreshWebRuntimeContext() {
	gpuMode := "enabled"
	if windowsWebview2GPUDisabled() {
		gpuMode = "disabled"
	}
	publishWebRuntimeContext(webRuntimeContext{
		Engine: "webview2", RuntimeVersion: webView2RuntimeVersion(), GPUMode: gpuMode,
	})
}
