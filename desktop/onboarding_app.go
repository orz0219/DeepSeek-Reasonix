package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"reasonix/internal/config"
)

// ConfirmAction shows a native confirmation dialog and returns true when the user
// clicks the confirm button. For destructive actions the dialog type is Warning so
// the platform can apply its danger styling (red tint on macOS, etc.).
func (a *App) ConfirmAction(req NativeConfirmRequest) (bool, error) {
	if a.ctx == nil {
		return false, nil
	}
	dialogType := runtime.QuestionDialog
	if req.Destructive {
		dialogType = runtime.WarningDialog
	}
	confirm := req.ConfirmLabel
	if confirm == "" {
		confirm = "OK"
	}
	cancel := req.CancelLabel
	if cancel == "" {
		cancel = "Cancel"
	}
	title := req.Title
	if title == "" {
		title = req.Message
	}
	body := req.Message
	if req.Detail != "" {
		if body != "" {
			body += "\n\n" + req.Detail
		} else {
			body = req.Detail
		}
	}
	defaultBtn := confirm
	if req.Destructive {

		defaultBtn = cancel
	}
	result, err := runtime.MessageDialog(a.ctx, runtime.MessageDialogOptions{
		Type:          dialogType,
		Title:         title,
		Message:       body,
		Buttons:       []string{confirm, cancel},
		DefaultButton: defaultBtn,
		CancelButton:  cancel,
	})
	if err != nil {
		return false, err
	}
	return result == confirm, nil
}

func (a *App) NeedsOnboarding() bool {
	cfg, err := config.LoadForRootReadOnly(a.activeWorkspaceRoot())
	if err != nil {

		return false
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !modelProviderAccessAllowed(cfg.Desktop.ProviderAccess, p.Name) || !p.Configured() || len(p.ChatModelList()) == 0 {
			continue
		}
		return false
	}
	return true
}

// ConnectKey validates apiKey against the balance endpoint, persists it to
// Reasonix's global .env, and rebuilds the controller so the new key takes effect.
func (a *App) ConnectKey(apiKey string) (string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", fmt.Errorf("key is required")
	}
	if tab := a.activeTab(); tab != nil {
		if err := rebuildControllerActiveWorkErrorFor(tab.Ctrl, "provider key"); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(a.ctx, 8*time.Second)
	defer cancel()
	if _, err := connectKeyBalanceFetch(ctx, nil, onboardingBalanceURL, apiKey); err != nil {
		return "", fmt.Errorf("validate: %w", err)
	}
	warning, err := a.saveProviderCredential(onboardingKeyEnv, apiKey)
	if err != nil {
		return "", fmt.Errorf("save: %w", err)
	}
	if err := a.ensureProviderAccessForKey(onboardingKeyEnv); err != nil {
		return "", fmt.Errorf("enable provider: %w", err)
	}
	if err := a.rebuildSetting("provider key"); err != nil {
		if rebuildWarning, ok := a.deferredRebuildWarning("provider key", err); ok {
			warning = appendSettingsWarning(warning, rebuildWarning)
		} else {
			return "", err
		}
	}
	return warning, nil
}
