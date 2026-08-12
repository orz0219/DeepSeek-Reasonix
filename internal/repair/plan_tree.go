package repair

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/config"
)

// repairPlanReleaseNodeState binds either a single-file identity or a full
// directory tree digest. Directory kind/mode alone is not enough for .app
// bundles: interior executables can change without touching the root node.
func repairPlanReleaseNodeState(path string) string {
	return repairPlanReleaseNodeStateFor(path, path)
}

func repairPlanReleaseNodeStateFor(readPath, identityPath string) string {
	info, err := os.Lstat(readPath)
	if err != nil {
		return repairPlanFileSnapshotFor(readPath, identityPath).StateID
	}
	if info.IsDir() {
		return repairPlanTreeStateIDFor(readPath, identityPath)
	}
	return repairPlanFileSnapshotFor(readPath, identityPath).StateID
}

func verifyRepairPlanReleaseNodeStateFor(readPath, identityPath, expected string) error {
	actual := repairPlanReleaseNodeStateFor(readPath, identityPath)
	if expected != actual {
		return fmt.Errorf("repair plan preview changed since confirmation; re-preview and re-confirm (expected %s, got %s)", expected, actual)
	}
	return nil
}

type repairPlanTreeEntry struct {
	Rel     string `json:"rel"`
	Kind    string `json:"kind"`
	Mode    uint32 `json:"mode,omitempty"`
	Content string `json:"content,omitempty"`
}

func repairPlanTreeEntries(root string) ([]repairPlanTreeEntry, error) {
	entries := make([]repairPlanTreeEntry, 0, 64)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			entries = append(entries, repairPlanTreeEntry{Rel: path, Kind: "unreadable"})
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		if rel == "." {
			rel = ""
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			entries = append(entries, repairPlanTreeEntry{Rel: rel, Kind: "unreadable"})
			return nil
		}
		entry := repairPlanTreeEntry{Rel: filepath.ToSlash(rel), Mode: uint32(info.Mode())}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			entry.Kind = "symlink"
			if target, readErr := os.Readlink(path); readErr == nil {
				entry.Content = target
			} else {
				entry.Kind = "symlink-unreadable"
			}
		case info.IsDir():
			entry.Kind = "directory"
		case info.Mode().IsRegular():
			entry.Kind = "file"
			if sum, hashErr := hashFile(path); hashErr == nil {
				entry.Content = sum
			} else {
				entry.Kind = "file-unreadable"
			}
		default:
			entry.Kind = "other"
		}
		entries = append(entries, entry)
		return nil
	})
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Rel == entries[j].Rel {
			return entries[i].Kind < entries[j].Kind
		}
		return entries[i].Rel < entries[j].Rel
	})
	return entries, walkErr
}

// repairPlanTreeContentStateID hashes a directory tree without binding its
// root path. This lets an update handoff prove that the staged bundle and the
// installed bundle contain the same bytes even though they live at different
// paths.
func repairPlanTreeContentStateID(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("expected directory, got %s", info.Mode().Type())
	}
	entries, err := repairPlanTreeEntries(root)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		switch entry.Kind {
		case "unreadable", "file-unreadable", "symlink-unreadable":
			return "", fmt.Errorf("cannot read bundle entry %q", entry.Rel)
		case "other":
			return "", fmt.Errorf("unsupported bundle entry %q", entry.Rel)
		}
	}
	return repairPlanStateID(entries), nil
}

func repairPlanTreeStateIDFor(readRoot, identityRoot string) string {
	entries, err := repairPlanTreeEntries(readRoot)
	if err != nil {
		entries = []repairPlanTreeEntry{{Rel: readRoot, Kind: "unreadable"}}
	}
	return repairPlanStateID(struct {
		Target  string                `json:"target"`
		Entries []repairPlanTreeEntry `json:"entries"`
	}{repairPlanTargetIdentity(identityRoot), entries})
}

func pendingUpdateFiles(tx *UpdateTransaction) []UpdateTransactionFile {
	if tx == nil {
		return nil
	}
	if len(tx.Files) > 0 {
		return tx.Files
	}
	return []UpdateTransactionFile{{
		TargetPath: tx.TargetPath,
		BackupPath: tx.BackupPath,
		SHA256:     tx.BackupSHA256,
	}}
}

func pendingUpdateTargetPaths(tx *UpdateTransaction) []string {
	if tx == nil {
		return nil
	}
	files := pendingUpdateFiles(tx)
	paths := make([]string, 0, len(files)+1)
	seen := map[string]struct{}{}
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for _, f := range files {
		add(f.TargetPath)
	}
	add(tx.TargetPath)
	if strings.EqualFold(strings.TrimSpace(tx.TargetKind), "app-bundle") {
		add(tx.BackupPath)
		add(tx.OrphanedBackupPath)
	}
	return paths
}

func repairPlanActionMutationPaths(action RepairPlanAction, opts ApplyPlanOptions) ([]string, error) {
	switch action.Type {
	case "repair_config":
		return configRepairTargetPaths(ConfigOptions{Root: opts.Root, IncludeProject: action.Scope == "project", OnlyScope: action.Scope})
	case "restore_snapshot":
		dir, contentPath, metadataPath, err := configSnapshotPaths(action.SnapshotID)
		if err != nil {
			return nil, err
		}
		return []string{config.UserConfigPath(), dir, contentPath, metadataPath}, nil
	case "rebuild_derived_state":
		return derivedStateTargetPaths(action.Target)
	default:
		return nil, nil
	}
}

func verifyRepairPlanFileState(path string, expectedStates map[string]string) error {
	if len(expectedStates) == 0 {
		return nil
	}
	expected, ok := expectedStates[path]
	if !ok {
		return fmt.Errorf("repair plan preview did not bind target state; re-preview and re-confirm")
	}
	return verifyRepairPlanStateID(path, expected)
}

func verifyRepairPlanFileStates(expectedStates map[string]string) error {
	if len(expectedStates) == 0 {
		return nil
	}
	paths := make([]string, 0, len(expectedStates))
	for path := range expectedStates {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := verifyRepairPlanFileState(path, expectedStates); err != nil {
			return err
		}
	}
	return nil
}

func verifyRepairPlanStateID(path, expected string) error {
	actual := repairPlanFileState(path)
	if expected != actual {
		return fmt.Errorf("repair plan preview changed since confirmation; re-preview and re-confirm (expected %s, got %s)", expected, actual)
	}
	return nil
}

func configSnapshotByID(id string) (ConfigSnapshot, error) {
	snapshots, err := ListConfigSnapshots()
	if err != nil {
		return ConfigSnapshot{}, err
	}
	for _, snap := range snapshots {
		if snap.ID == id {
			return snap, nil
		}
	}
	return ConfigSnapshot{}, fmt.Errorf("config snapshot %q not found", id)
}

func projectConfigPath(root string) string {
	root = strings.TrimSpace(root)
	if root == "" || root == "." {
		return "reasonix.toml"
	}
	return filepath.Join(root, "reasonix.toml")
}
