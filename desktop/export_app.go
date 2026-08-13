package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"reasonix/internal/control"
)

// SavePastedImage stores a browser clipboard image data URL under the active
// tab's workspace .reasonix/attachments and returns the relative @-reference path.
func (a *App) SavePastedImage(dataURL string) (string, error) {
	return a.withActiveWorkspace(func() (string, error) {
		return control.SaveImageDataURL(dataURL)
	})
}

// SaveClipboardImage reads the native OS clipboard image under the active tab's
// workspace .reasonix/attachments and returns the relative @-reference path.
func (a *App) SaveClipboardImage() (string, error) {
	return a.withActiveWorkspace(control.SaveClipboardImage)
}

// SavePastedFile stores a dropped non-image file (the browser exposes its bytes
// as a data URL but not a real path) under the active tab's workspace
// .reasonix/attachments and returns the relative @-reference path.
func (a *App) SavePastedFile(name, dataURL string) (string, error) {
	return a.withActiveWorkspace(func() (string, error) {
		return control.SaveAttachmentDataURL(name, dataURL)
	})
}

// PickExportFile opens the native save dialog and returns the selected path. It
// returns "" when the user cancels.
func (a *App) PickExportFile(defaultFilename, mimeType string) (string, error) {
	if a.ctx == nil {
		return "", nil
	}
	defaultFilename = safeExportFilename(defaultFilename)
	ext := strings.ToLower(filepath.Ext(defaultFilename))
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:                "Export session",
		DefaultDirectory:     dialogDefaultDirectory(a.activeWorkspaceRoot()),
		DefaultFilename:      defaultFilename,
		CanCreateDirectories: true,
		Filters:              exportFileFilters(mimeType, ext),
	})
	if err != nil || path == "" {
		return "", err
	}
	if ext != "" && filepath.Ext(path) == "" {
		path += ext
	}
	return path, nil
}

// SaveExportFile writes an exported session payload to a path previously picked
// by PickExportFile. An empty path is treated as a cancelled export.
func (a *App) SaveExportFile(path, payload string, base64Encoded bool) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	var data []byte
	var err error
	if base64Encoded {
		data, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return fmt.Errorf("decode export payload: %w", err)
		}
	} else {
		data = []byte(payload)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return exportOperationError("save export file", path, err)
	}
	return nil
}

// SaveExportImageFiles writes one or more base64-encoded image parts. A single
// image keeps the native save dialog's normal overwrite semantics. Multi-part
// exports use numbered sibling paths and never overwrite an existing sibling;
// every payload is staged before any target is committed, and a failed commit
// removes only files created by this call.
func (a *App) SaveExportImageFiles(path string, payloads []string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if len(payloads) == 0 {
		return errors.New("no image payloads to export")
	}
	if len(payloads) == 1 {
		return a.SaveExportFile(path, payloads[0], true)
	}

	targets := make([]string, len(payloads))
	for i := range payloads {
		targets[i] = numberedExportPath(path, i, len(payloads))
	}

	return saveExclusiveExportPayloads(targets, len(payloads), func(index int) ([]byte, error) {
		decoded, err := base64.StdEncoding.DecodeString(payloads[index])
		if err != nil {
			return nil, fmt.Errorf("decode export image part %d: %w", index+1, err)
		}
		return decoded, nil
	})
}

type stagedExportFile struct {
	targetPath string
	tempPath   string
}

type committedExportFile struct {
	path string
	info os.FileInfo
}

const exportTempCreateAttempts = 100

func numberedExportPath(path string, partIndex, partCount int) string {
	if partCount <= 1 {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	return fmt.Sprintf("%s-%d-of-%d%s", stem, partIndex+1, partCount, ext)
}

func saveExclusiveExportPayloads(targets []string, payloadCount int, payloadAt func(int) ([]byte, error)) error {
	if len(targets) == 0 || len(targets) != payloadCount || payloadAt == nil {
		return errors.New("invalid export image batch")
	}
	for _, target := range targets {
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("export file already exists: %s", filepath.Base(target))
		} else if !errors.Is(err, os.ErrNotExist) {
			return exportOperationError("inspect export target", target, err)
		}
	}

	staged := make([]stagedExportFile, 0, len(targets))
	defer func() {
		for _, file := range staged {
			_ = os.Remove(file.tempPath)
		}
	}()
	for i, target := range targets {
		payload, err := payloadAt(i)
		if err != nil {
			return err
		}
		file, finalMode, err := createExportTempFile(filepath.Dir(target))
		if err != nil {
			return exportOperationError("stage export file", target, err)
		}
		tempPath := file.Name()
		staged = append(staged, stagedExportFile{targetPath: target, tempPath: tempPath})
		if _, err = file.Write(payload); err == nil {
			err = file.Sync()
		}

		if err == nil {
			err = file.Chmod(finalMode)
		}
		if err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return exportOperationError("stage export file", target, err)
		}
	}

	committed := make([]committedExportFile, 0, len(staged))
	for _, file := range staged {
		info, err := commitStagedExportFile(file.tempPath, file.targetPath)
		if err != nil {
			rollbackCommittedExportFiles(committed)
			return exportOperationError("save export file", file.targetPath, err)
		}
		committed = append(committed, committedExportFile{path: file.targetPath, info: info})
	}
	return nil
}

// createExportTempFile reserves a cryptographically random sibling path with
// the same requested mode as a normal export. It immediately narrows the mode
// while bytes are staged; the caller restores finalMode only after the payload
// has been completely written and synced.
func createExportTempFile(dir string) (*os.File, os.FileMode, error) {
	for range exportTempCreateAttempts {
		var suffix [12]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, 0, fmt.Errorf("generate export temp name: %w", err)
		}
		path := filepath.Join(dir, ".reasonix-export-"+hex.EncodeToString(suffix[:]))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		info, err := file.Stat()
		if err == nil {
			err = file.Chmod(0o600)
		}
		if err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return nil, 0, err
		}
		return file, info.Mode().Perm(), nil
	}
	return nil, 0, errors.New("could not reserve a unique export temp file")
}

func commitStagedExportFile(tempPath, targetPath string) (os.FileInfo, error) {
	stagedInfo, err := os.Lstat(tempPath)
	if err != nil {
		return nil, err
	}

	if err := os.Link(tempPath, targetPath); err == nil {
		current, statErr := os.Lstat(targetPath)
		if statErr != nil {
			removeExportFileIfSame(targetPath, stagedInfo)
			return nil, statErr
		}
		if !os.SameFile(current, stagedInfo) {
			return nil, errors.New("export target changed while it was being saved")
		}
		return stagedInfo, nil
	}

	source, err := os.Open(tempPath)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	target, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	info, statErr := target.Stat()
	if statErr == nil {
		_, err = io.Copy(target, source)
	}
	if err == nil && statErr == nil {
		err = target.Sync()
	}
	if closeErr := target.Close(); err == nil && statErr == nil {
		err = closeErr
	}
	if statErr != nil {
		err = statErr
	}
	if err != nil {
		removeExportFileIfSame(targetPath, info)
		return nil, err
	}
	return info, nil
}

func rollbackCommittedExportFiles(files []committedExportFile) {
	for _, file := range files {
		removeExportFileIfSame(file.path, file.info)
	}
}

func removeExportFileIfSame(path string, created os.FileInfo) {
	if created == nil {
		return
	}
	current, err := os.Lstat(path)
	if err == nil && os.SameFile(current, created) {
		_ = os.Remove(path)
	}
}

func exportOperationError(operation, path string, err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%s %s: %w", operation, filepath.Base(path), pathErr.Err)
	}
	return fmt.Errorf("%s %s: %w", operation, filepath.Base(path), err)
}

func safeExportFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "reasonix-session.md"
	}
	return filepath.Base(name)
}

func exportFileFilters(mimeType, ext string) []runtime.FileFilter {
	switch mimeType {
	case "text/markdown":
		return []runtime.FileFilter{{DisplayName: "Markdown (*.md)", Pattern: "*.md"}}
	case "application/json":
		return []runtime.FileFilter{{DisplayName: "JSON (*.json)", Pattern: "*.json"}}
	case "application/pdf":
		return []runtime.FileFilter{{DisplayName: "PDF (*.pdf)", Pattern: "*.pdf"}}
	case "image/png":
		return []runtime.FileFilter{{DisplayName: "PNG image (*.png)", Pattern: "*.png"}}
	}
	if ext != "" {
		return []runtime.FileFilter{{DisplayName: strings.ToUpper(strings.TrimPrefix(ext, ".")) + " files (*" + ext + ")", Pattern: "*" + ext}}
	}
	return []runtime.FileFilter{{DisplayName: "All files (*.*)", Pattern: "*.*"}}
}

// AttachmentDataURL returns a safe data URL for a stored image attachment.
func (a *App) AttachmentDataURL(path string) (string, error) {
	return a.withActiveWorkspace(func() (string, error) {
		return control.ImageDataURL(path)
	})
}

// DroppedItem is one OS-dropped file resolved into a composer context entry: an
// in-tree file becomes a workspace @reference (read in place, no copy), while an
// outside directory becomes a session-scoped workspace @reference; an image or
// out-of-tree file is copied into .reasonix/attachments.
type DroppedItem struct {
	Kind        string `json:"kind"` // "workspace" | "attachment"
	Path        string `json:"path"`
	IsDir       bool   `json:"isDir,omitempty"`
	DisplayPath string `json:"displayPath,omitempty"`
	PreviewURL  string `json:"previewUrl,omitempty"`
}

// AttachDropped turns an absolute path from the native file-drop bridge into a
// composer context entry. Images are stored as attachments so the chip shows a
// thumbnail; in-workspace files are referenced relatively (no copy); directories
// outside the workspace are registered as current-session folder references;
// files outside the workspace are copied into .reasonix/attachments.
func (a *App) AttachDropped(path string) (DroppedItem, error) {
	var item DroppedItem
	err := a.withActiveWorkspaceDo(func() error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if isImageExt(path) {
			if rel, err := control.SaveImageFile(path); err == nil {
				preview, _ := control.ImageDataURL(rel)
				item = DroppedItem{Kind: "attachment", Path: rel, PreviewURL: preview}
				return nil
			}
		}
		if rel, ok := workspaceRelativeIn(path, a.activeWorkspaceRoot()); ok {
			item = DroppedItem{Kind: "workspace", Path: rel, IsDir: info.IsDir()}
			return nil
		}
		if info.IsDir() {
			tab, ctrl := a.tabAndCtrlByID("")
			if err := a.ensureTabControllerWorkspace(tab); err != nil {
				return err
			}
			if tab != nil {
				ctrl = a.controllerForTab(tab)
			}
			if ctrl == nil {
				return fmt.Errorf("workspace is not ready")
			}
			token, displayPath, err := ctrl.RegisterExternalFolderRef(path)
			if err != nil {
				return err
			}
			item = DroppedItem{Kind: "workspace", Path: token, IsDir: true, DisplayPath: displayPath}
			return nil
		}
		rel, err := control.SaveAttachmentFile(path)
		if err != nil {
			return err
		}
		item = DroppedItem{Kind: "attachment", Path: rel}
		return nil
	})
	if err != nil {
		return DroppedItem{}, err
	}
	return item, nil
}
