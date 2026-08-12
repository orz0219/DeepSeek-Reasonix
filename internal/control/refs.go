package control

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// maxFileRefBytes caps how much of an @-referenced file is injected into a
// message, so "@somehuge.log" can't blow the context window. The head is kept
// and the rest noted as truncated.
const maxFileRefBytes = 64 * 1024

const pdfExtractTimeout = 8 * time.Second
const pdfExtractWaitDelay = 1 * time.Second

var extractPDFText = extractPDFTextDefault

type pdfExtractResult struct {
	text      string
	tool      string
	truncated bool
}

// refKind distinguishes the two things an @reference can resolve to.
type refKind int

const (
	refResource refKind = iota // an MCP resource: @<server>:<uri>
	refFile                    // a local file or directory: @<path>
	refImage                   // a local image attachment: @.reasonix/attachments/<file>
)

// ref is a resolved @reference found in a submitted line.
type ref struct {
	kind        refKind
	server      string // refResource
	uri         string // refResource
	path        string // refFile, relative to baseDir when baseDir is set
	baseDir     string // refFile override for session-authorized external roots
	displayPath string // refFile label/path exposed in the resolved context block
	raw         string // the original token after '@', for labelling
}

// ExternalFolderRefEntry is a session-authorized entry under a dropped external
// folder. Path is the opaque @ token path to submit; display fields are safe for
// UI labels and transcripts.
type ExternalFolderRefEntry struct {
	Name        string
	Path        string
	DisplayName string
	DisplayPath string
	IsDir       bool
}

var pathLocationSuffixRe = regexp.MustCompile(`:\d+(?::\d+)?:?$`)

const externalFolderRefPrefix = "__reasonix_external_folder"

// parseRefTokens extracts the deduped, punctuation-trimmed tokens following '@'
// in a line. A token is a run of non-whitespace bytes, except that a
// backslash-escaped space or tab is part of the token with the backslash
// dropped — that is how a path containing spaces survives the
// whitespace-delimited grammar (EscapeRefPath produces that form). Any other
// backslash stays literal so Windows separators keep their meaning. Pure:
// classification (server? file?) happens in classifyRef.
func parseRefTokens(line string) []string {
	var toks []string
	seen := map[string]bool{}
	for i := 0; i < len(line); i++ {
		if line[i] != '@' {
			continue
		}
		var b strings.Builder
		j := i + 1
		for j < len(line) {
			ch := line[j]
			if ch == '\\' && j+1 < len(line) && (line[j+1] == ' ' || line[j+1] == '\t') {
				b.WriteByte(line[j+1])
				j += 2
				continue
			}
			if isRefTokenBoundary(ch) {
				break
			}
			b.WriteByte(ch)
			j++
		}
		i = j - 1
		t := strings.TrimRight(b.String(), ".,;!?)]}")
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		toks = append(toks, t)
	}
	return toks
}

// isRefTokenBoundary matches the whitespace class the old `@([^\s]+)` token
// regexp stopped at.
func isRefTokenBoundary(ch byte) bool {
	switch ch {
	case ' ', '\t', '\n', '\r', '\f':
		return true
	default:
		return false
	}
}

// EscapeRefPath returns path with spaces and tabs backslash-escaped so the
// result survives whitespace-delimited @-token parsing (parseRefTokens
// reverses it). Every other byte, including backslashes, passes through
// unchanged so Windows separators keep their meaning.
func EscapeRefPath(path string) string {
	if !strings.ContainsAny(path, " \t") {
		return path
	}
	var b strings.Builder
	b.Grow(len(path) + 8)
	for i := range len(path) {
		if path[i] == ' ' || path[i] == '\t' {
			b.WriteByte('\\')
		}
		b.WriteByte(path[i])
	}
	return b.String()
}

// UnescapeRefPath reverses EscapeRefPath: a backslash before a space or tab is
// dropped; any other backslash stays literal.
func UnescapeRefPath(path string) string {
	if !strings.Contains(path, `\`) {
		return path
	}
	var b strings.Builder
	b.Grow(len(path))
	for i := range len(path) {
		if path[i] == '\\' && i+1 < len(path) && (path[i+1] == ' ' || path[i+1] == '\t') {
			continue
		}
		b.WriteByte(path[i])
	}
	return b.String()
}

// classifyRef decides what a token refers to. A "server:uri" token whose server
// is connected is an MCP resource; otherwise a token that names an existing path
// is a file. Anything else (an @mention, an email) is not a reference. exists is
// injected so the rule is testable without touching the filesystem.
func classifyRef(token string, known map[string]bool, exists func(string) bool) (ref, bool) {
	if i := strings.Index(token, ":"); i > 0 && i+1 < len(token) && known[token[:i]] {
		return ref{kind: refResource, server: token[:i], uri: token[i+1:], raw: token}, true
	}
	if isAttachmentRef(token) && exists(token) {
		if isImageAttachmentRef(token) {
			return ref{kind: refImage, path: token, raw: token}, true
		}
		return ref{kind: refFile, path: token, raw: token}, true
	}
	if exists(token) {
		return ref{kind: refFile, path: token, raw: token}, true
	}
	return ref{}, false
}

func isAttachmentRef(token string) bool {
	return strings.HasPrefix(filepath.ToSlash(token), ".reasonix/attachments/")
}

func isImageAttachmentRef(token string) bool {
	switch strings.ToLower(filepath.Ext(token)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg", ".tif", ".tiff":
		return true
	}
	return false
}

// RegisterExternalFolderRef authorizes one dropped directory outside the
// workspace as a structured @reference for this controller session. The returned
// token is path-like and whitespace-free so it survives the existing @ token
// parser even when the real directory path contains spaces or Windows drive
// punctuation.
func (c *Controller) RegisterExternalFolderRef(path string) (token, displayPath string, err error) {
	if c == nil {
		return "", "", fmt.Errorf("controller is not ready")
	}
	abs, err := normalizeExternalFolderRoot(path)
	if err != nil {
		return "", "", err
	}
	token = externalFolderRefToken(abs)
	c.externalFolderRefsMu.Lock()
	if c.externalFolderRefs == nil {
		c.externalFolderRefs = map[string]string{}
	}
	c.externalFolderRefs[token] = abs
	c.externalFolderRefsMu.Unlock()
	if c.externalFolderToolRefs != nil {
		c.externalFolderToolRefs.RegisterReadRoot(token, abs)
	}
	return token, filepath.ToSlash(abs), nil
}

func normalizeExternalFolderRoot(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", os.ErrInvalid
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(resolved)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return abs, nil
}

func externalFolderRefToken(abs string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	hash := hex.EncodeToString(sum[:])[:12]
	name := safeExternalFolderRefComponent(filepath.Base(abs))
	return externalFolderRefPrefix + "/" + hash + "/" + name
}

func safeExternalFolderRefComponent(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "folder"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "folder"
	}
	return out
}

func normalizeExternalFolderRefToken(token string) string {
	token = strings.TrimSpace(token)
	token = strings.TrimPrefix(token, "@")
	token = filepath.ToSlash(token)
	token = strings.TrimRight(token, "/")
	return token
}

func (c *Controller) externalFolderRef(token string) (ref, bool) {
	_, rel, abs, ok := c.externalFolderRefTarget(token)
	if !ok {
		return ref{}, false
	}
	displayPath := externalFolderDisplayPath(abs, rel)
	return ref{kind: refFile, path: rel, baseDir: abs, displayPath: displayPath, raw: token}, true
}

func (c *Controller) externalFolderRefTarget(token string) (rootToken, rel, abs string, ok bool) {
	key := normalizeExternalFolderRefToken(token)
	if !strings.HasPrefix(key, externalFolderRefPrefix+"/") {
		return "", "", "", false
	}
	c.externalFolderRefsMu.RLock()
	defer c.externalFolderRefsMu.RUnlock()
	if abs, ok := c.externalFolderRefs[key]; ok {
		return key, ".", abs, true
	}
	for registered, abs := range c.externalFolderRefs {
		if !strings.HasPrefix(key, registered+"/") {
			continue
		}
		sub, ok := cleanExternalFolderSubpath(strings.TrimPrefix(key, registered+"/"))
		if !ok {
			return "", "", "", false
		}
		return registered, sub, abs, true
	}
	return "", "", "", false
}

func cleanExternalFolderSubpath(sub string) (string, bool) {
	sub = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(sub)), "/")
	if sub == "" || sub == "." {
		return ".", true
	}
	cleaned := filepath.Clean(filepath.FromSlash(sub))
	if cleaned == "." {
		return ".", true
	}
	if !filepath.IsLocal(cleaned) {
		return "", false
	}
	return filepath.ToSlash(cleaned), true
}

func externalFolderDisplayPath(abs, rel string) string {
	if rel == "" || rel == "." {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(filepath.Join(abs, filepath.FromSlash(rel)))
}

func externalFolderDisplayName(abs, rel string) string {
	name := filepath.Base(abs)
	if rel != "" && rel != "." {
		name = filepath.ToSlash(filepath.Join(name, filepath.FromSlash(rel)))
	}
	return name
}

// ListExternalFolderRefDir lists one directory level under a registered
// external folder token. handled is true only when tokenPath targets a
// registered external folder; callers can fall back to workspace listing when it
// is false.

// SearchExternalFolderRefs finds entries under all registered external folders.
// Returned Path values are opaque token paths, so selecting one stays within the
// current session's authorization boundary.

// ExternalFolderRefLocalPath resolves a registered external-folder token path to
// the local filesystem path authorized for this controller session.

// detectRefs finds the @references in a line: MCP resources for connected
// servers, and local paths that exist on disk.

// HasRefs reports whether a line contains any resolvable @references, so a
// frontend can decide to resolve off its event loop only when needed.

// inputImages resolves image @-references in the turn input to data URLs so the
// turn can carry them to a vision-capable model. Best-effort: an unreadable image
// is skipped — the @ref still lands as text via ResolveRefs.

// resolveInputImageCandidates resolves authorized image references without
// consulting the active model capability. The parent controller uses this only
// to hand candidates to a child; the child decides whether to embed them.

// resolveBareNames batch-resolves simple filenames (no path separator) that
// don't exist in cwd. It walks the working tree once and matches every
// unresolved name against the set, stopping when all are found. This runs in
// the async ResolveRefs path, never on the TUI event loop.

// FileRefLine reports whether a submitted line is nothing but a path to an
// existing file — a dragged or pasted file lands as its bare path, which on
// POSIX starts with '/' and would otherwise be misread as a slash command. The
// returned string is that path turned into an @reference so it attaches.

// SlashCodeCommentLine reports whether a slash-prefixed line is ordinary source
// text rather than a Reasonix slash command.

// SlashPathLineRef reports whether a slash-prefixed line starts with a local file
// path, including common compiler-location suffixes like ":12" or ":12:34".
// It returns an @reference for the file so diagnostics that begin with an
// absolute path can keep their original text while also attaching file context.

// SlashPathLikeLine reports whether a slash-prefixed line looks like a POSIX
// absolute path rather than a slash command. It intentionally stays conservative:
// unknown "/foo" remains an unknown command, while "/foo/bar..." is sent as
// ordinary prompt text even if the path no longer exists.

// ResolveRefs resolves the @references in a line into a single tagged context
// block (file/dir contents, MCP resource bodies), plus per-reference error
// strings for any that failed. An empty block means no references resolved.
// Safe to call off a frontend's event loop; honours ctx for the resource reads.

// ResolveScopedRefs is the HTTP/frontend variant: file references are honored
// only when they can be resolved under the controller workspace root.

// maxDirEntries caps how many directory entries are injected so @some-huge-dir
// can't blow the context window.

// readFileRef reads an @-referenced path for injection. A directory yields a
// recursive listing capped at maxDirEntries; a binary file (NUL in the first
// 8 KiB) is noted rather than dumped; a large file is truncated to
// maxFileRefBytes with a marker. isDir lets the caller pick the wrapping tag.
// When baseDir is non-empty the read is sandboxed under it via os.Root so
// user-supplied paths cannot escape the workspace; otherwise the path is
// used as-is (CLI single-workspace compatibility).

// readFileRefUnscoped is the legacy readFileRef body kept for CLI single-workspace
// compatibility, where no controller-scoped sandbox is in effect.

// walkRootDir walks a directory under a sandboxed *os.Root and writes each
// entry relative to base (skipping noisy ones like .git and node_modules) into b
// until n hits maxDirEntries.

// resolveAbsRef resolves the user-supplied @-reference path against baseDir
// and returns the absolute path plus the absolute base root to sandbox I/O
// under. With a baseDir, the path is confined under it (a relative path that
// escapes via ".." is rejected). With an empty baseDir, the path is returned
// as-is and the caller falls back to plain os.Stat/os.Open so CLI usage
// (where there is no controller-scoped workspace) keeps working.
