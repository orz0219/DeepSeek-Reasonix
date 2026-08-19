// Store-level revision counter: a monotonically increasing integer persisted
// alongside memory files so sessions can detect cross-session mutations without
// watching the filesystem. The counter increments on every Save/Archive/Delete
// and is the sum of mutations across both GlobalDir and Dir.
package memory

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
)

const revisionFile = ".memory_revision"

// revisionMu serialises revision reads and writes within a single process.
// The file-level atomicity handles cross-process safety.
var revisionMu sync.Mutex

// CurrentRevision returns the monotonic revision counter for this store,
// which is the max revision across both scope directories. A zero store
// returns 0.
func (s Store) CurrentRevision() int {
	revisionMu.Lock()
	defer revisionMu.Unlock()
	max := 0
	for _, dir := range s.dirs() {
		if dir == "" {
			continue
		}
		if v := readRevisionFile(dir); v > max {
			max = v
		}
	}
	return max
}

// bumpRevision atomically increments the revision counter in the given
// directory. It is called after every successful mutation (Save, Archive,
// Delete) so the store's revision always reflects the latest write.
func bumpRevision(dir string) {
	if dir == "" {
		return
	}
	revisionMu.Lock()
	defer revisionMu.Unlock()
	path := revisionPath(dir)
	raw, err := fileencoding.ReadFileUTF8(path)
	current := 0
	if err == nil {
		current, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	if current < 1 {
		current = 1
	} else {
		current++
	}
	_ = fileutil.AtomicWriteFile(path, []byte(strconv.Itoa(current)), 0o644)
}

// readRevisionFile reads the revision counter from a directory. Missing or
// unparseable files return 0.
func readRevisionFile(dir string) int {
	raw, err := fileencoding.ReadFileUTF8(revisionPath(dir))
	if err != nil {
		return 0
	}
	v, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if v < 0 {
		return 0
	}
	return v
}

func revisionPath(dir string) string {
	return filepath.Join(dir, revisionFile)
}

// ensureRevisionFile creates the revision file with value 1 if it does not
// exist yet. Called during MigrateV2 so pre-existing stores start with a
// valid revision.
func ensureRevisionFile(dir string) {
	if dir == "" {
		return
	}
	path := revisionPath(dir)
	if _, err := os.Stat(path); err == nil {
		return
	}
	_ = fileutil.AtomicWriteFile(path, []byte("1"), 0o644)
}
