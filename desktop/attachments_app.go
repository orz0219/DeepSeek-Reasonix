package main

import (
	"path/filepath"
	"strings"
)

func isImageExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

func workspaceRelativeIn(path, workspaceRoot string) (string, bool) {
	root := workspaceRoot
	if !filepath.IsAbs(root) {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "", false
		}
		root = abs
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

type MemoryImport struct {
	Path       string `json:"path"`
	SourcePath string `json:"sourcePath"`
}

// MemoryDoc is one resolved instruction file with applicability metadata.
type MemoryDoc struct {
	Path       string         `json:"path"`
	Scope      string         `json:"scope"`
	Directory  string         `json:"directory,omitempty"`
	Body       string         `json:"body"`
	Imports    []MemoryImport `json:"imports"`
	Depth      int            `json:"depth"`
	Order      int            `json:"order"`
	Precedence int            `json:"precedence"`
}

type InstructionDiagnostic struct {
	Code       string `json:"code"`
	Path       string `json:"path"`
	SourcePath string `json:"sourcePath,omitempty"`
	Line       int    `json:"line,omitempty"`
	Message    string `json:"message"`
}

// MemoryFact is one saved auto-memory, surfaced read-only in the panel.
type MemoryFact struct {
	ID          string `json:"id,omitempty"`
	Revision    int    `json:"revision,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Scope       string `json:"scope"`
	Body        string `json:"body"`
	Freshness   string `json:"freshness"`
}

type MemoryConflict struct {
	Key         string `json:"key"`
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	GlobalID    string `json:"globalId"`
	GlobalName  string `json:"globalName"`
	Resolution  string `json:"resolution"`
}

type MemoryRecallHit struct {
	ID        string  `json:"id"`
	Revision  int     `json:"revision"`
	Name      string  `json:"name"`
	Title     string  `json:"title,omitempty"`
	Type      string  `json:"type"`
	Scope     string  `json:"scope"`
	Score     float64 `json:"score"`
	Freshness string  `json:"freshness"`
	Reason    string  `json:"reason"`
	Snippet   string  `json:"snippet"`
}

type MemoryRecallTrace struct {
	Query      string            `json:"query"`
	Hits       []MemoryRecallHit `json:"hits"`
	Omitted    int               `json:"omitted"`
	CharBudget int               `json:"charBudget"`
	UsedChars  int               `json:"usedChars"`
	Suppressed string            `json:"suppressed,omitempty"`
}

// MemoryArchive is one archived auto-memory kept only for inspection.
type MemoryArchive struct {
	ID          string `json:"id,omitempty"`
	Revision    int    `json:"revision,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"`
	UpdatedAt   string `json:"updatedAt,omitempty"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Scope       string `json:"scope"`
	Body        string `json:"body"`
	Freshness   string `json:"freshness"`
	Path        string `json:"path"`
	ArchivedAt  string `json:"archivedAt,omitempty"`
}

// MemoryScope is one writable quick-add target (scope id + the file it writes to).
type MemoryScope struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
}

// MemoryView is the whole memory panel payload: hierarchical docs, active saved
// facts, archived facts, and the writable scopes for the quick-add selector.
type MemoryView struct {
	Docs                   []MemoryDoc             `json:"docs"`
	Facts                  []MemoryFact            `json:"facts"`
	Archives               []MemoryArchive         `json:"archives"`
	Scopes                 []MemoryScope           `json:"scopes"`
	InstructionDiagnostics []InstructionDiagnostic `json:"instructionDiagnostics"`
	Conflicts              []MemoryConflict        `json:"conflicts"`
	LastRecall             MemoryRecallTrace       `json:"lastRecall"`
	StoreDir               string                  `json:"storeDir"`
	StoreGlobalDir         string                  `json:"storeGlobalDir,omitempty"`
	Available              bool                    `json:"available"`
}

// writableScopes are the quick-add targets the panel offers, broad → specific.
var writableScopes = []string{"user", "project", "local"}
