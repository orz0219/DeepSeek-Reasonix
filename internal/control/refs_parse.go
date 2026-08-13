package control

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// HasRefs reports whether a line contains any resolvable @references, so a
// frontend can decide to resolve off its event loop only when needed.
func (c *Controller) HasRefs(line string) bool {
	return len(c.detectRefs(line)) > 0
}

// inputImages resolves image @-references in the turn input to data URLs so the
// turn can carry them to a vision-capable model. Best-effort: an unreadable image
// is skipped — the @ref still lands as text via ResolveRefs.

// resolveInputImageCandidates resolves authorized image references without
// consulting the active model capability. The parent controller uses this only
// to hand candidates to a child; the child decides whether to embed them.
func (c *Controller) resolveInputImageCandidates(line string) []string {
	var urls []string
	for _, r := range c.detectRefs(line) {
		baseDir := c.workspaceRoot
		if r.baseDir != "" {
			baseDir = r.baseDir
		}
		if url, err := visionRefImageDataURL(r, baseDir); err == nil {
			urls = append(urls, url)
		}
	}
	return urls
}

func visionRefImageDataURL(r ref, baseDir string) (string, error) {
	switch r.kind {
	case refImage:
		return visionImageDataURL(r.path)
	case refFile:
		return visionFileImageDataURL(r.path, baseDir)
	default:
		return "", fmt.Errorf("reference is not an image")
	}
}

func visionFileImageDataURL(path, baseDir string) (string, error) {
	absPath, absBase, ok := resolveAbsRef(path, baseDir)
	if !ok {
		return "", os.ErrNotExist
	}
	if absBase == "" {
		return "", fmt.Errorf("workspace root is required for file image references")
	}

	root, err := os.OpenRoot(absBase)
	if err != nil {
		return "", err
	}
	defer root.Close()

	rel, err := filepath.Rel(absBase, absPath)
	if err != nil {
		return "", err
	}

	info, err := root.Lstat(rel)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("image path must not be a symlink")
	}
	if info.IsDir() || info.Size() <= 0 || info.Size() > maxImageAttachmentBytes {
		return "", fmt.Errorf("image must be between 1 byte and 10 MB")
	}
	f, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, opened) {
		return "", fmt.Errorf("image changed while opening")
	}
	return dataURLFromImageReader(f, path)
}

func dataURLFromImageReader(r io.Reader, path string) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxImageAttachmentBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || len(raw) > maxImageAttachmentBytes {
		return "", fmt.Errorf("image must be between 1 byte and 10 MB")
	}
	mime := detectedImageMime(raw)
	if mime == "" {
		return "", fmt.Errorf("%s is not a supported image", path)
	}
	raw, mime = compressForVision(raw, mime)
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// resolveBareNames batch-resolves simple filenames (no path separator) that
// don't exist in cwd. It walks the working tree once and matches every
// unresolved name against the set, stopping when all are found. This runs in
// the async ResolveRefs path, never on the TUI event loop.
func resolveBareNames(refs []ref, workspaceRoot string) []ref {
	need := map[string]*ref{}
	var names []string
	for i := range refs {
		r := &refs[i]
		if r.kind != refFile || r.path != "" || !isSafeBareRefName(r.raw) {
			continue
		}
		if workspaceRoot != "" {
			if rel, ok := workspaceRefPath(r.raw, workspaceRoot); ok {
				r.path = rel
				continue
			}
		}
		need[r.raw] = r
		names = append(names, r.raw)
	}
	if len(names) == 0 {
		return refs
	}
	found := 0
	cwd := workspaceRoot
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	_ = filepath.WalkDir(cwd, func(p string, d os.DirEntry, wErr error) error {
		if wErr != nil || found == len(names) {
			return filepath.SkipAll
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".DS_Store", "__pycache__", ".idea", ".vscode":
				return filepath.SkipDir
			}
			return nil
		}
		if r, ok := need[d.Name()]; ok {
			rel, _ := filepath.Rel(cwd, p)
			r.path = filepath.ToSlash(rel)
			delete(need, d.Name())
			found++
		}
		return nil
	})
	return refs
}

func isSafeBareRefName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	return filepath.Base(name) == name && filepath.IsLocal(name)
}

// FileRefLine reports whether a submitted line is nothing but a path to an
// existing file — a dragged or pasted file lands as its bare path, which on
// POSIX starts with '/' and would otherwise be misread as a slash command. The
// returned string is that path turned into an @reference so it attaches.
func FileRefLine(line string) (string, bool) {
	p := strings.Trim(strings.TrimSpace(line), `"'`)
	if p == "" {
		return "", false
	}
	if info, err := os.Stat(p); err != nil || info.IsDir() {
		return "", false
	}
	return "@" + EscapeRefPath(p), true
}

// SlashCodeCommentLine reports whether a slash-prefixed line is ordinary source
// text rather than a Reasonix slash command.
func SlashCodeCommentLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*")
}

// SlashPathLineRef reports whether a slash-prefixed line starts with a local file
// path, including common compiler-location suffixes like ":12" or ":12:34".
// It returns an @reference for the file so diagnostics that begin with an
// absolute path can keep their original text while also attaching file context.
func SlashPathLineRef(line, baseDir string) (string, bool) {
	token, ok := leadingSlashPathToken(line)
	if !ok {
		return "", false
	}
	for _, p := range pathTokenCandidates(token) {
		if fileRefExists(p, baseDir) {
			return "@" + p, true
		}
	}
	return "", false
}

// SlashPathLikeLine reports whether a slash-prefixed line looks like a POSIX
// absolute path rather than a slash command. It intentionally stays conservative:
// unknown "/foo" remains an unknown command, while "/foo/bar..." is sent as
// ordinary prompt text even if the path no longer exists.
func SlashPathLikeLine(line string) bool {
	token, ok := leadingSlashPathToken(line)
	if !ok {
		return false
	}
	for _, p := range pathTokenCandidates(token) {
		if strings.Contains(p[1:], "/") {
			return true
		}
	}
	return false
}

func leadingSlashPathToken(line string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return "", false
	}
	token := strings.Trim(fields[0], `"'`)
	if !strings.HasPrefix(token, "/") || strings.HasPrefix(token, "//") {
		return "", false
	}
	return token, true
}

func pathTokenCandidates(token string) []string {
	token = strings.TrimRight(strings.Trim(token, `"'`), ".,;!?)]}")
	if token == "" {
		return nil
	}
	candidates := []string{token}
	if stripped := pathLocationSuffixRe.ReplaceAllString(token, ""); stripped != token {
		candidates = append(candidates, stripped)
	}
	return candidates
}

func fileRefExists(path, baseDir string) bool {
	if baseDir != "" {
		rel, _, absBase, ok := workspaceRel(path, baseDir)
		if !ok {
			return false
		}
		root, err := os.OpenRoot(absBase)
		if err != nil {
			return false
		}
		defer root.Close()
		info, err := root.Stat(rel)
		return err == nil && !info.IsDir()
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func workspaceRefPath(path, baseDir string) (string, bool) {
	rel, _, absBase, ok := workspaceRel(path, baseDir)
	if !ok {
		return "", false
	}
	root, err := os.OpenRoot(absBase)
	if err != nil {
		return "", false
	}
	defer root.Close()
	if _, err := root.Stat(rel); err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func workspaceRel(path, baseDir string) (rel, absPath, absBase string, ok bool) {
	absPath, absBase, ok = resolveAbsRef(path, baseDir)
	if !ok || absBase == "" {
		return "", "", "", false
	}
	rel, err := filepath.Rel(absBase, absPath)
	if err != nil || !filepath.IsLocal(rel) {
		return "", "", "", false
	}
	return rel, absPath, absBase, true
}
