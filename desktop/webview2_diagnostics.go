package main

import "sync/atomic"

// webview2_diagnostics.go keeps the native WebView2 process-failure event type
// and observer-installed flag used by the Windows observer. Report generation
// and metrics were removed.

type webView2NativeEvent struct {
	Kind                int
	Reason              int
	ReasonAvailable     bool
	ExitCode            int32
	ExitCodeAvailable   bool
	ProcessDescription  string
	FailureSourceModule string
	Recovery            string
}

var nativeWebView2ObserverInstalled atomic.Bool

func webView2NativeObserverInstalled() bool {
	return nativeWebView2ObserverInstalled.Load()
}
