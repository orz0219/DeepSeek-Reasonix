package control

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/fileref"
)

// ListExternalFolderRefDir lists one directory level under a registered
// external folder token. handled is true only when tokenPath targets a
// registered external folder; callers can fall back to workspace listing when it
// is false.
func (c *Controller) ListExternalFolderRefDir(tokenPath string) (entries []ExternalFolderRefEntry, handled bool) {
	rootToken, rel, abs, ok := c.externalFolderRefTarget(tokenPath)
	if !ok {
		return nil, false
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, true
	}
	defer root.Close()
	info, err := root.Stat(rel)
	if err != nil || !info.IsDir() {
		return nil, true
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, true
	}
	dirEntries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return nil, true
	}
	dirs, files := []ExternalFolderRefEntry{}, []ExternalFolderRefEntry{}
	for _, e := range dirEntries {
		name := e.Name()
		if skipRefDirEntry(name, e.IsDir()) {
			continue
		}
		childRel := name
		if rel != "." {
			childRel = filepath.ToSlash(filepath.Join(rel, name))
		}
		item := ExternalFolderRefEntry{
			Name:        name,
			Path:        rootToken + "/" + childRel,
			DisplayName: name,
			DisplayPath: externalFolderDisplayPath(abs, childRel),
			IsDir:       e.IsDir(),
		}
		if e.IsDir() {
			dirs = append(dirs, item)
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, item)
	}
	sortExternalFolderRefEntries(dirs)
	sortExternalFolderRefEntries(files)
	return append(dirs, files...), true
}

// SearchExternalFolderRefs finds entries under all registered external folders.
// Returned Path values are opaque token paths, so selecting one stays within the
// current session's authorization boundary.
func (c *Controller) SearchExternalFolderRefs(query string, limit int) []ExternalFolderRefEntry {
	query = strings.TrimSpace(query)
	if limit <= 0 || len(query) < 2 || strings.ContainsAny(query, `/\`) {
		return nil
	}
	c.externalFolderRefsMu.RLock()
	roots := make([]struct {
		token string
		abs   string
	}, 0, len(c.externalFolderRefs))
	for token, abs := range c.externalFolderRefs {
		roots = append(roots, struct {
			token string
			abs   string
		}{token: token, abs: abs})
	}
	c.externalFolderRefsMu.RUnlock()
	sort.Slice(roots, func(i, j int) bool {
		return externalFolderDisplayPath(roots[i].abs, ".") < externalFolderDisplayPath(roots[j].abs, ".")
	})
	out := make([]ExternalFolderRefEntry, 0, limit)
	queryLower := strings.ToLower(query)
	for _, root := range roots {
		if len(out) >= limit {
			break
		}
		if info, err := os.Stat(root.abs); err != nil || !info.IsDir() {
			continue
		}
		if strings.Contains(strings.ToLower(filepath.Base(root.abs)), queryLower) {
			out = append(out, ExternalFolderRefEntry{
				Name:        filepath.Base(root.abs),
				Path:        root.token,
				DisplayName: externalFolderDisplayName(root.abs, "."),
				DisplayPath: externalFolderDisplayPath(root.abs, "."),
				IsDir:       true,
			})
			if len(out) >= limit {
				break
			}
		}
		for _, result := range fileref.Search(root.abs, query, limit-len(out)) {
			rel := filepath.ToSlash(result.Path)
			out = append(out, ExternalFolderRefEntry{
				Name:        rel,
				Path:        root.token + "/" + rel,
				DisplayName: externalFolderDisplayName(root.abs, rel),
				DisplayPath: externalFolderDisplayPath(root.abs, rel),
				IsDir:       result.IsDir,
			})
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// ExternalFolderRefLocalPath resolves a registered external-folder token path to
// the local filesystem path authorized for this controller session.
func (c *Controller) ExternalFolderRefLocalPath(tokenPath string) (path, displayPath string, ok bool) {
	_, rel, abs, ok := c.externalFolderRefTarget(tokenPath)
	if !ok {
		return "", "", false
	}
	return filepath.Join(abs, filepath.FromSlash(rel)), externalFolderDisplayPath(abs, rel), true
}

func sortExternalFolderRefEntries(entries []ExternalFolderRefEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].DisplayName) < strings.ToLower(entries[j].DisplayName)
	})
}

func skipRefDirEntry(name string, isDir bool) bool {
	switch name {
	case ".DS_Store", "Thumbs.db":
		return true
	}
	if !isDir {
		return false
	}
	switch name {
	case ".codex", ".git", ".idea", ".npm", ".pnpm-store", ".vscode", "__pycache__", "build", "dist", "node_modules":
		return true
	}
	return false
}

// detectRefs finds the @references in a line: MCP resources for connected
// servers, and local paths that exist on disk.
func (c *Controller) detectRefs(line string) []ref {
	return c.detectRefsMode(line, false)
}

func (c *Controller) detectRefsMode(line string, scopedOnly bool) []ref {
	known := map[string]bool{}
	for _, n := range c.mcp.serverNames() {
		known[n] = true
	}

	var refs []ref
	for _, tok := range parseRefTokens(line) {
		if i := strings.Index(tok, ":"); i > 0 && i+1 < len(tok) && known[tok[:i]] {
			refs = append(refs, ref{kind: refResource, server: tok[:i], uri: tok[i+1:], raw: tok})
			continue
		}
		if r, ok := c.externalFolderRef(tok); ok {
			refs = append(refs, r)
			continue
		}
		if c.workspaceRoot != "" {
			if rel, ok := workspaceRefPath(tok, c.workspaceRoot); ok {
				kind := refFile
				if isAttachmentRef(rel) && isImageAttachmentRef(rel) {
					kind = refImage
				}
				refs = append(refs, ref{kind: kind, path: rel, raw: tok})
			}
			continue
		}
		if scopedOnly {
			continue
		}
		if r, ok := classifyRef(tok, known, func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		}); ok {
			refs = append(refs, r)
		}
	}
	return refs
}
