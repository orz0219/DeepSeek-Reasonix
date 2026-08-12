package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/instruction"
)

// ResolveRefs resolves the @references in a line into a single tagged context
// block (file/dir contents, MCP resource bodies), plus per-reference error
// strings for any that failed. An empty block means no references resolved.
// Safe to call off a frontend's event loop; honours ctx for the resource reads.
func (c *Controller) ResolveRefs(ctx context.Context, line string) (block string, errs []string) {
	return c.resolveRefs(ctx, line, false)
}

// ResolveScopedRefs is the HTTP/frontend variant: file references are honored
// only when they can be resolved under the controller workspace root.
func (c *Controller) ResolveScopedRefs(ctx context.Context, line string) (block string, errs []string) {
	return c.resolveRefs(ctx, line, true)
}

func (c *Controller) resolveRefs(ctx context.Context, line string, scopedOnly bool) (block string, errs []string) {
	refs := c.detectRefsMode(line, scopedOnly)
	refs = resolveBareNames(refs, c.workspaceRoot)
	var b strings.Builder
	includedInstructionPaths := map[string]bool{}
	includedInstructionBodies := map[string]bool{}
	if current := c.memory.current(); current != nil {
		for _, doc := range current.Docs {
			includedInstructionPaths[cleanAbsPath(doc.Path)] = true
			includedInstructionBodies[doc.Body] = true
		}
	}
	for _, r := range refs {
		switch r.kind {
		case refResource:
			text, err := c.mcp.readResource(ctx, r.server, r.uri)
			if err != nil {
				errs = append(errs, "@"+r.raw+" — "+err.Error())
				continue
			}
			appendRefBlock(&b, "resource", `ref="@`+r.raw+`"`, text)
		case refFile:
			baseDir := c.workspaceRoot
			if r.baseDir != "" {
				baseDir = r.baseDir
			}
			text, isDir, err := readFileRef(r.path, baseDir)
			if err != nil {
				errs = append(errs, "@"+r.raw+" — "+err.Error())
				continue
			}
			pathInstructions, diagnostics := c.resolveReferencedInstructions(r, baseDir, includedInstructionPaths, includedInstructionBodies)
			if pathInstructions != "" {
				appendRefBlock(&b, "path-instructions", `target="`+html.EscapeString(displayPathForRef(r))+`"`, pathInstructions)
			}
			for _, diagnostic := range diagnostics {
				errs = append(errs, "@"+r.raw+" — "+diagnostic.Message)
			}
			tag := "file"
			if isDir {
				tag = "dir"
			}
			displayPath := r.path
			if r.displayPath != "" {
				displayPath = r.displayPath
			}
			appendRefBlock(&b, tag, `path="`+displayPath+`"`, text)
		case refImage:
			appendRefBlock(&b, "image", `path="`+r.path+`"`, "[image attachment available at @"+r.path+"; sent as direct model image input only when the selected model supports vision. Text-only models can still use an available OCR/image/vision tool with this local path; image bytes are not inlined into prompt text.]")
		}
	}
	return b.String(), errs
}

func (c *Controller) resolveReferencedInstructions(r ref, baseDir string, includedPaths, includedBodies map[string]bool) (string, []instruction.Diagnostic) {
	mem := c.memory.current()
	if mem == nil || strings.TrimSpace(c.workspaceRoot) == "" || r.baseDir != "" {
		return "", nil
	}
	absPath, absBase, ok := resolveAbsRef(r.path, baseDir)
	if !ok || cleanAbsPath(absBase) != cleanAbsPath(c.workspaceRoot) {
		return "", nil
	}
	targetDir := absPath
	if info, err := os.Stat(absPath); err != nil || !info.IsDir() {
		targetDir = filepath.Dir(absPath)
	}
	resolved := instruction.Resolve(instruction.ResolveOptions{
		WorkspaceRoot: c.workspaceRoot,
		TargetDir:     targetDir,
		UserDir:       mem.UserDir,
	})
	var delta []instruction.Document
	for _, doc := range resolved.Documents {
		pathKey := cleanAbsPath(doc.Path)
		if includedPaths[pathKey] || includedBodies[doc.Body] {
			continue
		}
		includedPaths[pathKey] = true
		includedBodies[doc.Body] = true
		delta = append(delta, doc)
	}
	return instruction.Block(delta), resolved.Diagnostics
}

func displayPathForRef(r ref) string {
	if r.displayPath != "" {
		return r.displayPath
	}
	return r.path
}

func cleanAbsPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

func appendRefBlock(b *strings.Builder, tag, attr, body string) {
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	fmt.Fprintf(b, "<%s %s>\n%s\n</%s>", tag, attr, body, tag)
}

// maxDirEntries caps how many directory entries are injected so @some-huge-dir
// can't blow the context window.
const maxDirEntries = 100

const maxDirDepth = 16

func directoryRefNote() string {
	return fmt.Sprintf("[directory listing only; file contents are not inlined. Mention a listed file path to read its content. Common generated/vendor folders are skipped. Listing is capped at %d entries and %d nested levels.]", maxDirEntries, maxDirDepth)
}

// readFileRef reads an @-referenced path for injection. A directory yields a
// recursive listing capped at maxDirEntries; a binary file (NUL in the first
// 8 KiB) is noted rather than dumped; a large file is truncated to
// maxFileRefBytes with a marker. isDir lets the caller pick the wrapping tag.
// When baseDir is non-empty the read is sandboxed under it via os.Root so
// user-supplied paths cannot escape the workspace; otherwise the path is
// used as-is (CLI single-workspace compatibility).
func readFileRef(path, baseDir string) (content string, isDir bool, err error) {
	absPath, absBase, ok := resolveAbsRef(path, baseDir)
	if !ok {
		return "", false, os.ErrNotExist
	}
	if absBase == "" {
		return readFileRefUnscoped(absPath)
	}

	root, rerr := os.OpenRoot(absBase)
	if rerr != nil {
		return "", false, rerr
	}
	defer root.Close()

	rel, rerr := filepath.Rel(absBase, absPath)
	if rerr != nil {
		return "", false, rerr
	}
	displayPath := filepath.ToSlash(rel)

	info, err := root.Stat(rel)
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		var b strings.Builder
		b.WriteString(directoryRefNote())
		b.WriteString("\n\n")
		n := 0
		err := walkRootDir(root, rel, rel, &b, &n, 0)
		if n >= maxDirEntries {
			b.WriteString("\n…[truncated; directory has more entries]…")
		}
		if err != nil {
			return "", true, err
		}
		return b.String(), true, nil
	}

	if strings.EqualFold(filepath.Ext(rel), ".pdf") {
		return readPDFRef(absPath, info.Size()), false, nil
	}

	f, err := root.Open(rel)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	buf := make([]byte, maxFileRefBytes+1)
	n, rerr := io.ReadFull(f, buf)
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		return "", false, rerr
	}
	data := buf[:n]

	if mime := imageMime(data, rel); mime != "" {
		return imageFileRefNote(displayPath, mime, info.Size(), true), false, nil
	}
	if bytes.IndexByte(data[:min(n, 8192)], 0) >= 0 {
		return fmt.Sprintf("[binary file %s, %d bytes — not shown]", displayPath, info.Size()), false, nil
	}
	if n > maxFileRefBytes {
		return string(data[:maxFileRefBytes]) + fmt.Sprintf("\n…[truncated; file is %d bytes]…", info.Size()), false, nil
	}
	return string(data), false, nil
}

// readFileRefUnscoped is the legacy readFileRef body kept for CLI single-workspace
// compatibility, where no controller-scoped sandbox is in effect.
func readFileRefUnscoped(path string) (content string, isDir bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		var b strings.Builder
		b.WriteString(directoryRefNote())
		b.WriteString("\n\n")
		n := 0
		err := filepath.WalkDir(path, func(p string, d os.DirEntry, wErr error) error {
			if wErr != nil {
				return wErr
			}
			if n >= maxDirEntries {
				return filepath.SkipAll
			}
			if p == path {
				return nil
			}
			if skipRefDirEntry(d.Name(), d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			rel, rErr := filepath.Rel(path, p)
			if rErr != nil {
				rel = p
			}
			rel = strings.ReplaceAll(rel, string(os.PathSeparator), "/")
			if d.IsDir() {
				rel += "/"
			}
			b.WriteString(rel)
			b.WriteByte('\n')
			n++
			return nil
		})
		if n >= maxDirEntries {
			b.WriteString("\n…[truncated; directory has more entries]…")
		}
		if err != nil {
			return "", true, err
		}
		return b.String(), true, nil
	}

	if strings.EqualFold(filepath.Ext(path), ".pdf") {
		return readPDFRef(path, info.Size()), false, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()

	buf := make([]byte, maxFileRefBytes+1)
	n, rerr := io.ReadFull(f, buf)
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		return "", false, rerr
	}
	data := buf[:n]

	if mime := imageMime(data, path); mime != "" {
		return imageFileRefNote(path, mime, info.Size(), false), false, nil
	}
	if bytes.IndexByte(data[:min(n, 8192)], 0) >= 0 {
		return fmt.Sprintf("[binary file %s, %d bytes — not shown]", path, info.Size()), false, nil
	}
	if n > maxFileRefBytes {
		return string(data[:maxFileRefBytes]) + fmt.Sprintf("\n…[truncated; file is %d bytes]…", info.Size()), false, nil
	}
	return string(data), false, nil
}

func imageFileRefNote(displayPath, mime string, size int64, attached bool) string {
	if attached {
		return fmt.Sprintf("[image file %s, mime=%s, %d bytes — sent as direct model image input only when the selected model supports vision. Text-only models can still use an available OCR/image/vision tool with this local path; image bytes are not inlined into prompt text.]", displayPath, mime, size)
	}
	return fmt.Sprintf("[image file %s, mime=%s, %d bytes — not sent as direct model image input because no workspace root is available. Use a workspace-scoped file reference, image attachment, or an available OCR/image/vision tool with a readable local path.]", displayPath, mime, size)
}

// walkRootDir walks a directory under a sandboxed *os.Root and writes each
// entry relative to base (skipping noisy ones like .git and node_modules) into b
// until n hits maxDirEntries.
func walkRootDir(root *os.Root, dir, base string, b *strings.Builder, n *int, depth int) error {
	if depth > maxDirDepth || *n >= maxDirEntries {
		return nil
	}
	f, err := root.Open(dir)
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	for _, e := range entries {
		if *n >= maxDirEntries {
			return nil
		}
		name := e.Name()
		child := filepath.ToSlash(filepath.Join(dir, name))
		entry := name
		if rel, err := filepath.Rel(base, child); err == nil && filepath.IsLocal(rel) {
			entry = filepath.ToSlash(rel)
		}
		if skipRefDirEntry(name, e.IsDir()) {
			continue
		}
		if e.IsDir() {
			entry += "/"
		}
		b.WriteString(entry)
		b.WriteByte('\n')
		*n++
		if e.IsDir() {
			if err := walkRootDir(root, child, base, b, n, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
