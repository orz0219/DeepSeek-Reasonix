package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"reasonix/internal/bot/weixin"
	"reasonix/internal/config"
)

func feishuAccountsBase(domain string) string {
	if domain == "lark" {
		return "https://accounts.larksuite.com"
	}
	return "https://accounts.feishu.cn"
}

func feishuRegistrationQRCodeURL(rawURL string) (string, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	query := parsedURL.Query()
	query.Set("from", "sdk")
	query.Set("tp", "sdk")
	query.Set("source", "go-sdk")
	parsedURL.RawQuery = query.Encode()
	return parsedURL.String(), nil
}

func postFeishuInstallForm(base string, body map[string]string) (map[string]any, error) {
	data, status, err := postFeishuInstallFormResult(base, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", status, firstNonEmptyBot(stringValue(data["error_description"]), stringValue(data["message"])))
	}
	return data, nil
}

func postFeishuInstallFormResult(base string, body map[string]string) (map[string]any, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reqBody := url.Values{}
	for k, v := range body {
		reqBody.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/oauth/v1/app/registration", strings.NewReader(reqBody.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

func botConnectionView(conn config.BotConnectionConfig) BotConnectionView {
	return BotConnectionView{
		ID: conn.ID, Provider: conn.Provider, Domain: conn.Domain, Label: conn.Label, Enabled: conn.Enabled, Status: conn.Status,
		Model: conn.Model, ToolApprovalMode: normalizeBotConnectionToolApprovalMode(conn.ToolApprovalMode), WorkspaceRoot: conn.WorkspaceRoot,
		Access: botAccessViewFromConfig(conn.Access),
		Credential: BotConnectionCredentialView{
			AppID: conn.Credential.AppID, AppSecretEnv: conn.Credential.AppSecretEnv, AccountID: conn.Credential.AccountID, TokenEnv: conn.Credential.TokenEnv,
			SecretSet: botCredentialSecretSet(conn),
		},
		SessionMappings: botSessionMappingViews(conn.SessionMappings, conn.WorkspaceRoot),
		LastError:       conn.LastError, CreatedAt: conn.CreatedAt, UpdatedAt: conn.UpdatedAt,
	}
}

func botCredentialSecretSet(conn config.BotConnectionConfig) bool {
	if conn.Credential.AppSecretEnv != "" {
		return envIsSet(conn.Credential.AppSecretEnv)
	}
	if conn.Credential.TokenEnv != "" && envIsSet(conn.Credential.TokenEnv) {
		return true
	}
	if conn.Provider == "weixin" {
		return weixin.HasSavedAccount(conn.Credential.AccountID)
	}
	return false
}

func feishuInstallDomain(fallback string, data map[string]any) string {
	if userInfo, ok := data["user_info"].(map[string]any); ok {
		if strings.EqualFold(stringValue(userInfo["tenant_brand"]), "lark") {
			return "lark"
		}
		return "feishu"
	}
	if strings.EqualFold(fallback, "lark") {
		return "lark"
	}
	return "feishu"
}

func feishuInstallUserID(data map[string]any) string {
	if userInfo, ok := data["user_info"].(map[string]any); ok {
		return firstNonEmptyBot(
			stringValue(userInfo["open_id"]),
			stringValue(userInfo["union_id"]),
			stringValue(userInfo["user_id"]),
		)
	}
	return ""
}

func botConnectionViews(connections []config.BotConnectionConfig) []BotConnectionView {
	if connections == nil {
		return []BotConnectionView{}
	}
	out := make([]BotConnectionView, 0, len(connections))
	for _, conn := range connections {
		out = append(out, botConnectionView(conn))
	}
	return out
}

func botConnectionConfig(view BotConnectionView) config.BotConnectionConfig {
	return config.BotConnectionConfig{
		ID:               strings.TrimSpace(view.ID),
		Provider:         strings.TrimSpace(view.Provider),
		Domain:           strings.TrimSpace(view.Domain),
		Label:            strings.TrimSpace(view.Label),
		Enabled:          view.Enabled,
		Status:           strings.TrimSpace(view.Status),
		Model:            strings.TrimSpace(view.Model),
		ToolApprovalMode: firstNonEmptyBot(normalizeBotConnectionToolApprovalMode(view.ToolApprovalMode), "ask"),
		WorkspaceRoot:    strings.TrimSpace(view.WorkspaceRoot),
		Access:           botAccessConfigFromView(view.Access),
		Credential: config.BotConnectionCredential{
			AppID:        strings.TrimSpace(view.Credential.AppID),
			AppSecretEnv: strings.TrimSpace(view.Credential.AppSecretEnv),
			AccountID:    strings.TrimSpace(view.Credential.AccountID),
			TokenEnv:     strings.TrimSpace(view.Credential.TokenEnv),
		},
		SessionMappings: botSessionMappingConfigs(view.SessionMappings, view.WorkspaceRoot),
		LastError:       strings.TrimSpace(view.LastError),
		CreatedAt:       strings.TrimSpace(view.CreatedAt),
		UpdatedAt:       strings.TrimSpace(view.UpdatedAt),
	}
}

func normalizeBotConnectionToolApprovalMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "ask":
		return "ask"
	case "auto":
		return "auto"
	case "yolo", "full", "full-access", "bypass":
		return "yolo"
	default:
		return ""
	}
}

func botConnectionConfigs(views []BotConnectionView) []config.BotConnectionConfig {
	if views == nil {
		return nil
	}
	out := make([]config.BotConnectionConfig, 0, len(views))
	for _, view := range views {
		cfg := botConnectionConfig(view)
		if cfg.ID == "" || cfg.Provider == "" {
			continue
		}
		out = append(out, cfg)
	}
	return out
}

func botMappingScope(scope, workspaceRoot string) string {
	if strings.TrimSpace(scope) == "project" {
		return "project"
	}
	if strings.TrimSpace(workspaceRoot) != "" {
		return "project"
	}
	return "global"
}

func botMappingWorkspaceRoot(scope, workspaceRoot string) string {
	if botMappingScope(scope, workspaceRoot) != "project" {
		return ""
	}
	return strings.TrimSpace(workspaceRoot)
}

func botSessionMappingViews(mappings []config.BotConnectionSessionMapping, connectionWorkspaceRoot string) []BotConnectionSessionMappingView {
	if mappings == nil {
		return []BotConnectionSessionMappingView{}
	}
	out := make([]BotConnectionSessionMappingView, 0, len(mappings))
	for _, m := range mappings {
		workspaceRoot := firstNonEmptyBot(m.WorkspaceRoot, connectionWorkspaceRoot)
		scope := botMappingScope(m.Scope, workspaceRoot)
		out = append(out, BotConnectionSessionMappingView{
			RemoteID:      m.RemoteID,
			SessionID:     m.SessionID,
			SessionSource: m.SessionSource,
			ChatType:      m.ChatType,
			UserID:        m.UserID,
			ThreadID:      m.ThreadID,
			Scope:         scope,
			WorkspaceRoot: botMappingWorkspaceRoot(scope, workspaceRoot),
			UpdatedAt:     m.UpdatedAt,
		})
	}
	return out
}

func botSessionMappingConfigs(mappings []BotConnectionSessionMappingView, connectionWorkspaceRoot string) []config.BotConnectionSessionMapping {
	if mappings == nil {
		return nil
	}
	out := make([]config.BotConnectionSessionMapping, 0, len(mappings))
	for _, m := range mappings {
		workspaceRoot := firstNonEmptyBot(m.WorkspaceRoot, connectionWorkspaceRoot)
		scope := botMappingScope(m.Scope, workspaceRoot)
		out = append(out, config.BotConnectionSessionMapping{
			RemoteID:      strings.TrimSpace(m.RemoteID),
			SessionID:     strings.TrimSpace(m.SessionID),
			SessionSource: strings.TrimSpace(m.SessionSource),
			ChatType:      strings.TrimSpace(m.ChatType),
			UserID:        strings.TrimSpace(m.UserID),
			ThreadID:      strings.TrimSpace(m.ThreadID),
			Scope:         scope,
			WorkspaceRoot: botMappingWorkspaceRoot(scope, workspaceRoot),
			UpdatedAt:     strings.TrimSpace(m.UpdatedAt),
		})
	}
	return out
}

func connectionID(provider, domain string) string {
	return strings.Trim(strings.ToLower(provider+"-"+domain), "-")
}

func botInstallAccess(userID string) config.BotAccessConfig {
	userID = strings.TrimSpace(userID)
	access := config.BotAccessConfig{Enabled: true, PairingEnabled: true}
	if userID != "" {
		access.Users = []string{userID}
	}
	return access
}

func randomInstallID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("install-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func envIsSet(name string) bool {
	return strings.TrimSpace(name) != "" && strings.TrimSpace(os.Getenv(name)) != ""
}

func firstAny(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstNonEmptyBot(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func appendUniqueBotString(values []string, next string) []string {
	next = strings.TrimSpace(next)
	if next == "" {
		return values
	}
	for _, value := range values {
		if strings.TrimSpace(value) == next {
			return values
		}
	}
	return append(values, next)
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func intValue(value any, fallback int) int {
	switch v := value.(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func weixinInstallStatusMessage(status string) string {
	switch status {
	case "scaned":
		return "已扫码，请在微信里确认。"
	case "scaned_but_redirect":
		return "已扫码，正在切换微信授权节点。"
	default:
		return "等待扫码。"
	}
}
