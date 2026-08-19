package pluginpkg

import (
	"os"
	"path/filepath"
)

func applyClaudeCompatibility(root string, manifest *Manifest) ([]string, []CompatibilityIssue) {
	appendRootClaudeInstructions(root, manifest)
	return appendClaudeCompatibility(root, manifest)
}

func appendRootClaudeInstructions(root string, manifest *Manifest) {
	path := filepath.Join(root, claudeInstructions)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	// CLAUDE.md startup context is no longer added as a hook.
	// The file is recognized as a plugin instruction source.
}
