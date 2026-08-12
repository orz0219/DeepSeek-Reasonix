package main

import (
	"bytes"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"reasonix/internal/control"
	"reasonix/internal/fileref"
	fileenc "reasonix/internal/fileutil/encoding"
)

// DirEntry is one entry in the "@" file-reference menu.
type DirEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path,omitempty"`
	IsDir       bool   `json:"isDir"`
	DisplayName string `json:"displayName,omitempty"`
	DisplayPath string `json:"displayPath,omitempty"`
}

// FilePreview is a bounded, read-only file payload for the workspace side panel.
type FilePreview struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
	Kind      string `json:"kind,omitempty"`
	Mime      string `json:"mime,omitempty"`
	URL       string `json:"url,omitempty"`
	Err       string `json:"err,omitempty"`
}

type WorkspaceChangeView struct {
	Path             string   `json:"path"`
	OldPath          string   `json:"oldPath,omitempty"`
	Sources          []string `json:"sources"`
	GitStatus        string   `json:"gitStatus,omitempty"`
	Turns            []int    `json:"turns,omitempty"`
	LatestPrompt     string   `json:"latestPrompt,omitempty"`
	LatestTime       int64    `json:"latestTime,omitempty"`
	CanSessionRevert bool     `json:"canSessionRevert,omitempty"`
}

type WorkspaceChangesView struct {
	Files        []WorkspaceChangeView `json:"files"`
	GitAvailable bool                  `json:"gitAvailable"`
	GitErr       string                `json:"gitErr,omitempty"`
	GitBranch    string                `json:"gitBranch,omitempty"`
}

type WorkspaceChangeDetailView struct {
	Diff      *string `json:"diff,omitempty"`
	Source    string  `json:"source,omitempty"`
	Added     int     `json:"added,omitempty"`
	Removed   int     `json:"removed,omitempty"`
	Binary    bool    `json:"binary,omitempty"`
	Truncated bool    `json:"truncated,omitempty"`
}

const filePreviewLimit = 2 * 1024 * 1024 // 2 MiB — full file preview for the workspace panel
const fileRefSearchLimit = 20

var previewMediaMIMEs = map[string]string{
	".bmp":  "image/bmp",
	".gif":  "image/gif",
	".jpeg": "image/jpeg",
	".jpg":  "image/jpeg",
	".pdf":  "application/pdf",
	".png":  "image/png",
	".svg":  "image/svg+xml",
	".webp": "image/webp",
}

func trimUTF8PartialSuffix(data []byte) []byte {
	if utf8.Valid(data) {
		return data
	}
	for i := len(data) - 1; i >= 0 && len(data)-i <= utf8.UTFMax; i-- {
		if !utf8.RuneStart(data[i]) {
			continue
		}
		if !utf8.Valid(data[:i]) || utf8.FullRune(data[i:]) {
			return data
		}
		return data[:i]
	}
	return data
}

func previewMediaKind(path string) (kind string, mime string) {
	mime = previewMediaMIMEs[strings.ToLower(filepath.Ext(path))]
	if mime == "" {
		return "", ""
	}
	if strings.HasPrefix(mime, "image/") {
		return "image", mime
	}
	if mime == "application/pdf" {
		return "pdf", mime
	}
	return "", ""
}

func workspaceEntryRel(rel, name string) string {
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	if rel == "" || rel == "." {
		return name
	}
	return rel + "/" + name
}

func skipWorkspaceEntry(rel, name string, isDir bool) bool {
	return fileref.SkipEntry(workspaceEntryRel(rel, name), name, isDir)
}

func (a *App) activeWorkspaceBase() (string, error) {
	return workspaceBaseFromRoot(a.activeWorkspaceRoot())
}

func (a *App) workspaceTargetForTab(tabID string) (string, control.SessionAPI, bool) {
	tabID = strings.TrimSpace(tabID)
	a.mu.RLock()
	defer a.mu.RUnlock()
	tab := a.tabByIDLocked(tabID)
	if tab == nil {
		if tabID == "" {
			return ".", nil, true
		}
		return "", nil, false
	}
	return tab.WorkspaceRoot, tab.Ctrl, true
}

func workspaceBaseFromRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" || root == "." {
		return os.Getwd()
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return filepath.Clean(root), nil
}

func workspacePathForBase(base, rel string) (string, bool, error) {
	base = filepath.Clean(base)
	if rel == "" {
		return "", false, os.ErrInvalid
	}
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, rel)
	}
	path = filepath.Clean(path)
	r, err := filepath.Rel(base, path)
	if err != nil {
		return "", false, err
	}
	if r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", false, os.ErrPermission
	}
	return path, true, nil
}

// ListDir lists one directory level (directories first, then files, each
// alphabetical) for the "@" file-reference menu. rel resolves against the active
// tab workspace. The menu navigates one level at a time, never recursively —
// bounded for huge trees.
func (a *App) ListDir(rel string) []DirEntry {
	return a.ListDirForTab("", rel)
}

// ListDirForTab is the tab-scoped variant used by multi-tab frontend surfaces.
func (a *App) ListDirForTab(tabID, rel string) []DirEntry {
	root, ctrl, ok := a.workspaceTargetForTab(tabID)
	if !ok {
		return []DirEntry{}
	}
	if browser := externalFolderRefBrowserFromController(ctrl); browser != nil {
		if entries, handled := browser.ListExternalFolderRefDir(rel); handled {
			return externalFolderDirEntries(entries)
		}
	}
	base, err := workspaceBaseFromRoot(root)
	if err != nil {
		return []DirEntry{}
	}
	dir := base
	if rel != "" {
		path, ok, err := workspacePathForBase(base, rel)
		if err != nil || !ok {
			return []DirEntry{}
		}
		dir = path
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		return []DirEntry{}
	}
	dirs, files := []DirEntry{}, []DirEntry{}
	for _, e := range es {
		name := e.Name()
		if skipWorkspaceEntry(rel, name, e.IsDir()) {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, DirEntry{Name: name, IsDir: true})
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, DirEntry{Name: name, IsDir: false})
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name) })
	return append(dirs, files...)
}

// SearchFileRefs finds workspace files by basename for bare "@token" completion.
func (a *App) SearchFileRefs(query string) []DirEntry {
	return a.SearchFileRefsForTab("", query)
}

// SearchFileRefsForTab is the tab-scoped variant used by multi-tab frontend surfaces.
func (a *App) SearchFileRefsForTab(tabID, query string) []DirEntry {
	root, ctrl, ok := a.workspaceTargetForTab(tabID)
	if !ok {
		return []DirEntry{}
	}
	base, err := workspaceBaseFromRoot(root)
	if err != nil {
		return []DirEntry{}
	}
	results := fileref.Search(base, query, fileRefSearchLimit)
	out := make([]DirEntry, 0, len(results))
	for _, r := range results {
		out = append(out, DirEntry{Name: r.Path, IsDir: r.IsDir})
	}
	if browser := externalFolderRefBrowserFromController(ctrl); browser != nil {
		out = append(out, externalFolderDirEntries(browser.SearchExternalFolderRefs(query, fileRefSearchLimit))...)
	}
	return out
}

type externalFolderRefBrowser interface {
	ListExternalFolderRefDir(tokenPath string) ([]control.ExternalFolderRefEntry, bool)
	SearchExternalFolderRefs(query string, limit int) []control.ExternalFolderRefEntry
	ExternalFolderRefLocalPath(tokenPath string) (path, displayPath string, ok bool)
}

func externalFolderRefBrowserFromController(ctrl control.SessionAPI) externalFolderRefBrowser {
	if browser, ok := ctrl.(externalFolderRefBrowser); ok {
		return browser
	}
	return nil
}

func externalFolderDirEntries(entries []control.ExternalFolderRefEntry) []DirEntry {
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, DirEntry{
			Name:        e.Name,
			Path:        e.Path,
			IsDir:       e.IsDir,
			DisplayName: e.DisplayName,
			DisplayPath: e.DisplayPath,
		})
	}
	return out
}

func (a *App) workspaceOrExternalPathForTab(tabID, rel string) (string, bool, error) {
	root, ctrl, ok := a.workspaceTargetForTab(tabID)
	if !ok {
		return "", false, os.ErrNotExist
	}
	if browser := externalFolderRefBrowserFromController(ctrl); browser != nil {
		if path, _, ok := browser.ExternalFolderRefLocalPath(rel); ok {
			return path, true, nil
		}
	}
	base, err := workspaceBaseFromRoot(root)
	if err != nil {
		return "", false, err
	}
	return workspacePathForBase(base, rel)
}

// ReadFile returns a small text preview for a file under the current workspace
// or a session-authorized external folder ref.
func (a *App) ReadFile(rel string) FilePreview {
	return a.ReadFileForTab("", rel)
}

// ReadFileForTab returns a preview resolved against the requested tab.
func (a *App) ReadFileForTab(tabID, rel string) FilePreview {
	out := FilePreview{Path: rel}
	path, ok, err := a.workspaceOrExternalPathForTab(tabID, rel)
	if err != nil || !ok {
		out.Err = "invalid path"
		return out
	}
	info, err := os.Stat(path)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	if info.IsDir() {
		out.Err = "path is a directory"
		return out
	}
	if !info.Mode().IsRegular() {
		out.Err = "path is not a regular file"
		return out
	}
	out.Size = info.Size()
	if kind, mime := previewMediaKind(path); kind != "" {
		token := a.ensureMediaTokenStore().create(path, info.Name(), mime, kind, info.Size(), info.ModTime())
		out.Kind = kind
		out.Mime = mime
		out.URL = "/__reasonix_workspace_media/" + token + "/" + url.PathEscape(info.Name())
		return out
	}
	f, err := os.Open(path)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	defer f.Close()

	buf := make([]byte, filePreviewLimit+1)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		out.Err = err.Error()
		return out
	}
	data := buf[:n]
	if len(data) > filePreviewLimit {
		data = data[:filePreviewLimit]
		out.Truncated = true
	}

	bomKind := fileenc.DetectQuick(data)
	if bomKind != fileenc.UTF8 {
		enc, _ := fileenc.Detect(data)
		if enc == fileenc.LossyUTF8 {
			out.Binary = true
			return out
		}
		decoded := fileenc.Decode(data, enc)
		out.Body = string(decoded)
		return out
	}

	if bytes.Contains(data, []byte{0}) {
		out.Binary = true
		return out
	}

	if out.Truncated {
		data = trimUTF8PartialSuffix(data)
	}
	enc, _ := fileenc.Detect(data)
	if enc == fileenc.LossyUTF8 {
		out.Binary = true
		return out
	}
	out.Body = string(fileenc.Decode(data, enc))
	return out
}

// OpenWorkspacePath opens a workspace or authorized external-ref file/folder in
// the OS default app.
func (a *App) OpenWorkspacePath(rel string) error {
	return a.OpenWorkspacePathForTab("", rel)
}

// OpenWorkspacePathForTab opens a path resolved against the requested tab.
func (a *App) OpenWorkspacePathForTab(tabID, rel string) error {
	path, ok, err := a.workspaceOrExternalPathForTab(tabID, rel)
	if err != nil || !ok {
		return os.ErrInvalid
	}
	return openWorkspacePath(path)
}

// RevealWorkspacePath shows a workspace or authorized external-ref file in the
// native file manager.
func (a *App) RevealWorkspacePath(rel string) error {
	return a.RevealWorkspacePathForTab("", rel)
}

// RevealWorkspacePathForTab reveals a path resolved against the requested tab.
func (a *App) RevealWorkspacePathForTab(tabID, rel string) error {
	path, ok, err := a.workspaceOrExternalPathForTab(tabID, rel)
	if err != nil || !ok {
		return os.ErrInvalid
	}
	return revealPath(path)
}

// RevealPath shows an arbitrary absolute path in the native file manager.
func (a *App) RevealPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return os.ErrInvalid
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return revealPath(path)
}

var revealPath = defaultRevealPath

func defaultRevealPath(path string) error {
	switch goruntime.GOOS {
	case "darwin":
		return exec.Command("open", "-R", path).Start()
	case "windows":

		explorer := "explorer.exe"
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = os.Getenv("windir")
		}
		if root != "" {
			explorer = filepath.Join(root, "explorer.exe")
		}
		return exec.Command(explorer, "/select,", path).Start()
	default:
		dir := path
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			dir = filepath.Dir(path)
		}
		return exec.Command("xdg-open", dir).Start()
	}
}
