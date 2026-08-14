package main

import (
	"github.com/wailsapp/wails/v2/pkg/logger"
)

// wails_logger.go adapts Wails' default logger for the desktop app. The
// WebView2 failure capture that used to feed crash/metrics reporting was
// removed; logging is local only.

type crashCaptureLogger struct {
	delegate logger.Logger
	app      *App
}

func newCrashCaptureLogger(app *App) logger.Logger {
	return &crashCaptureLogger{delegate: logger.NewDefaultLogger(), app: app}
}

func (l *crashCaptureLogger) Print(message string)   { l.delegate.Print(message) }
func (l *crashCaptureLogger) Trace(message string)   { l.delegate.Trace(message) }
func (l *crashCaptureLogger) Debug(message string)   { l.delegate.Debug(message) }
func (l *crashCaptureLogger) Info(message string)    { l.delegate.Info(message) }
func (l *crashCaptureLogger) Warning(message string) { l.delegate.Warning(message) }
func (l *crashCaptureLogger) Fatal(message string)   { l.delegate.Fatal(message) }
func (l *crashCaptureLogger) Error(message string)   { l.delegate.Error(message) }
