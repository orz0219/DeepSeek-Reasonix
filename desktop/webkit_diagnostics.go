package main

// webkit_diagnostics.go keeps the native WebKit termination event type used by
// the Linux process observer. Report generation and metrics were removed.

type webKitNativeEvent struct {
	reason         int
	recovery       int
	generation     uint64
	runtimeContext webRuntimeContext
}
