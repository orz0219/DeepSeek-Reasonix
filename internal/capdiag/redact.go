package capdiag

import (
	"strings"

	"reasonix/internal/secrets"
)

func sanitizeErr(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeErrText(err.Error())
}

// sanitizeErrText redacts secrets and machine-local identity from diagnostic
// strings. Prefer sanitizeErrTextWithPaths when workspace/home are known.
func sanitizeErrText(s string) string {
	return sanitizeErrTextWithPaths(s, "", "", "")
}

func sanitizeErrTextWithPaths(s, workspace, home, reasonixHome string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}

	s = strings.Join(strings.Fields(s), " ")

	if i := strings.IndexAny(s, "?#"); i >= 0 {

		prefix := s[:i]
		if strings.Contains(prefix, "://") || strings.Contains(strings.ToLower(prefix), "http") {
			s = prefix
		}
	}

	s = redactKeyValue(s, "PATH=")
	s = redactKeyValue(s, "path=")

	s = secrets.Redact(s)

	s = redactBearer(s)

	s = redactAbsolutePaths(s, workspace, home, reasonixHome)

	// Cap length after redaction.
	const max = 400
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func redactKeyValue(s, key string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, key)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(key)
		b.WriteString("<redacted>")
		rest := s[i+len(key):]
		end := len(rest)
		if j := strings.IndexAny(rest, " \t\n\r;,"); j >= 0 {
			end = j
		}
		s = rest[end:]
	}
}

func redactBearer(s string) string {
	var b strings.Builder
	lower := strings.ToLower(s)
	const needle = "bearer "
	for {
		i := strings.Index(lower, needle)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString("Bearer <redacted>")
		rest := s[i+len(needle):]
		end := len(rest)
		if j := strings.IndexAny(rest, " \t\n\r;,\"'"); j >= 0 {
			end = j
		}
		s = rest[end:]
		lower = strings.ToLower(s)
	}
}

func redactAbsolutePaths(s, workspace, home, reasonixHome string) string {
	// Walk for POSIX and Windows absolute path-like tokens.
	var b strings.Builder
	i := 0
	for i < len(s) {

		start := -1
		if s[i] == '/' {
			start = i
		} else if i+2 < len(s) && ((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) && s[i+1] == ':' && (s[i+2] == '\\' || s[i+2] == '/') {
			start = i
		}
		if start < 0 {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := start + 1
		for j < len(s) {
			c := s[j]
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '"' || c == '\'' || c == ',' || c == ';' || c == ')' || c == ']' {
				break
			}
			j++
		}
		token := s[start:j]

		if strings.ContainsAny(token, `/\`) && len(token) > 1 {
			b.WriteString(displayPath(token, workspace, home, reasonixHome))
		} else {
			b.WriteString(token)
		}
		i = j
	}
	return b.String()
}

func redactCommandDisplay(cmd, root, home, reasonixHome string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}

	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	return displayPath(fields[0], root, home, reasonixHome)
}
