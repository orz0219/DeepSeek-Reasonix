package main

import (
	"regexp"
	"strings"
)

// crash_app.go retains the local text-scrubbing helpers used by on-disk crash
// diagnostics (crash-fatal dumps and hang-watchdog logs); all network crash
// reporting (crash.reasonix.io) was removed.

const maxCrashDetailBytes = 16 << 10
const maxCrashStackBytes = 8 << 10
const maxCrashFieldBytes = 4 << 10

var (
	userPathSegment       = regexp.MustCompile(`(?i)([A-Z]:\\Users\\|/(?:home|Users)/)[^/\\:\s"']+`)
	emailPattern          = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	secretKeyValuePattern = regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|authorization|secret|password|passwd|pwd|token)\b\s*[:=]\s*(?:Bearer\s+)?['"]?[^'"\s,;]+['"]?`)
	bearerTokenPattern    = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{16,}`)
	explicitKeyPattern    = regexp.MustCompile(`\b(?:sk|rk)-(?:proj-)?[A-Za-z0-9_-]{16,}\b`)
	envIdentifierPattern  = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*(?:API[_-]?KEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY|SECRET|TOKEN|PASSWORD|PASSWD|PWD)[A-Z0-9_]*\b`)
	jwtPattern            = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
	longHexPattern        = regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)
	longBase64Pattern     = regexp.MustCompile(`[A-Za-z0-9+/]{40,}={0,2}`)
	longBase64URLPattern  = regexp.MustCompile(`\b[A-Za-z0-9_-]{48,}\b`)
)

func scrubUserPaths(s string) string {
	return userPathSegment.ReplaceAllString(s, "${1}_")
}

func scrubSensitiveText(s string) string {
	s = scrubUserPaths(s)
	s = emailPattern.ReplaceAllString(s, "[redacted-email]")
	s = bearerTokenPattern.ReplaceAllString(s, "Bearer [redacted]")
	s = secretKeyValuePattern.ReplaceAllString(s, "${1}=[redacted]")
	s = envIdentifierPattern.ReplaceAllString(s, "[redacted-env]")
	s = jwtPattern.ReplaceAllString(s, "[redacted-jwt]")
	s = explicitKeyPattern.ReplaceAllString(s, "[redacted-key]")
	s = longHexPattern.ReplaceAllString(s, "[redacted-hex]")
	s = longBase64Pattern.ReplaceAllString(s, "[redacted-token]")
	s = longBase64URLPattern.ReplaceAllString(s, "[redacted-token]")
	return s
}

func clipCrashField(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

func sanitizeCrashField(s string, max int) string {
	return clipCrashField(scrubUserPaths(strings.TrimSpace(s)), max)
}

func sanitizeCrashText(s string, max int) string {
	return clipCrashField(strings.TrimSpace(scrubSensitiveText(s)), max)
}
