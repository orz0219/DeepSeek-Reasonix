package main

import "sync/atomic"

// webview2_diagnostics.go keeps the native-observer-installed flag used by the
// Windows observer. Report generation and metrics were removed.

var nativeWebView2ObserverInstalled atomic.Bool
