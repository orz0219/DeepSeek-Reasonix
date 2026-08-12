package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"reasonix/internal/fileutil"
)

// WritePendingUpdate is retained for source compatibility with older repair
// callers. Pending transactions are immutable once created; callers that need
// to start an update should use the prepare APIs, and callers that need to
// transition one must use the exact transaction helpers below.
//
// Deprecated: this function only creates a pending transaction and refuses to
// replace an existing one.
func WritePendingUpdate(tx *UpdateTransaction) error {
	return createPendingUpdate(tx)
}

func createPendingUpdate(tx *UpdateTransaction) error {
	return writePendingUpdate(tx, true)
}

func writePendingUpdate(tx *UpdateTransaction, createOnly bool) error {
	if tx == nil {
		return fmt.Errorf("pending update: nil transaction")
	}
	path := PendingUpdatePath()
	if path == "" {
		return fmt.Errorf("pending update: Reasonix state directory is unavailable")
	}
	b, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return err
	}
	if createOnly {
		return fileutil.AtomicCreateFile(path, append(b, '\n'), 0o600)
	}
	return fileutil.AtomicWriteFile(path, append(b, '\n'), 0o600)
}

func removePendingUpdateExactVerified(expected *UpdateTransaction, verify func() error) error {
	if expected == nil {
		return fmt.Errorf("clear pending update: transaction identity is incomplete")
	}
	path := PendingUpdatePath()
	pendingUpdateBeforeCleanup(path)
	cleanup, err := moveRepairNodeToUniqueCleanup(path)
	if err != nil {
		return err
	}
	if cleanup == "" {
		return fmt.Errorf("clear pending update: pending transaction disappeared before commit")
	}
	updateCleanupAfterRename(path, cleanup)
	restore := func(cause error) error {
		if restoreErr := renameRepairNodeNoReplace(cleanup, path); restoreErr != nil {
			return fmt.Errorf("%w; pending transaction retained at %s: %w", cause, cleanup, restoreErr)
		}
		return cause
	}
	b, err := os.ReadFile(cleanup)
	if err != nil {
		return restore(err)
	}
	var actual UpdateTransaction
	if err := json.Unmarshal(b, &actual); err != nil {
		return restore(err)
	}
	if UpdateTransactionID(&actual) != UpdateTransactionID(expected) {
		return restore(fmt.Errorf("clear pending update: pending transaction changed"))
	}
	if verify != nil {
		if err := verify(); err != nil {
			return restore(err)
		}
	}
	if err := removePendingUpdateFile(cleanup); err != nil {
		return restore(err)
	}
	cleanupOrphanedAppBundleUpdateBackup(expected)
	return nil
}

// ensureNoPendingUpdate runs with the pending-update lock held. Preparing a new
// transaction over an existing one would overwrite fixed backup paths before
// the new transaction is durable, destroying the previous rollback material if
// preparation later fails.
func ensureNoPendingUpdate() error {
	disposition, tx, err := classifyPendingUpdate()
	if err != nil {
		return fmt.Errorf("prepare update: %w", err)
	}
	switch disposition {
	case pendingUpdateActionable:
		if tx == nil {

			if _, err := quarantinePendingUpdate("not valid for this installation"); err != nil {
				return fmt.Errorf("prepare update: quarantine unusable transaction: %w", err)
			}
			return nil
		}
		return fmt.Errorf("prepare update: a pending update already exists")
	case pendingUpdateDebris:

		if _, err := quarantinePendingUpdate("blocked a new update"); err != nil {
			return fmt.Errorf("prepare update: quarantine unusable transaction: %w", err)
		}
	}
	return nil
}

// pendingUpdateDisposition is what the pending-update marker on disk currently
// means. Preparation and reconciliation both classify through it so they cannot
// disagree about whether a transaction exists — when they did, a marker that
// reconciliation could not act on still made preparation refuse, and updates
// stayed blocked permanently (#7342).
type pendingUpdateDisposition int

// classifyPendingUpdate reads the marker and decides what can be done with it.
//
// The line between debris and an actionable transaction is deliberately drawn
// at self-description. A transaction that cannot be parsed, or that does not
// say which release it targets, for which platform, and when it was opened,
// names nothing to roll back to — discarding it loses nothing. Every other
// validation failure is environment-relative (the launcher path, whether the
// target sits inside this Guard installation, where the backup lives) and can
// fail for a perfectly good transaction observed from the wrong install, so
// those keep the old refusal rather than risking real rollback material.
//
// IO failures are errors, never debris: an unreadable marker is not an absent
// one, and quarantining on a transient permission error would throw away a
// recoverable transaction.
func classifyPendingUpdate() (pendingUpdateDisposition, *UpdateTransaction, error) {
	path := PendingUpdatePath()
	if path == "" {
		return pendingUpdateNone, nil, fmt.Errorf("Reasonix state directory is unavailable")
	}
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return pendingUpdateNone, nil, nil
		}
		return pendingUpdateNone, nil, fmt.Errorf("inspect pending transaction: %w", err)
	}
	tx, err := readPendingUpdateUnchecked()
	if err != nil {
		switch {
		case os.IsNotExist(err):
			return pendingUpdateNone, nil, nil
		case isPendingUpdateContentError(err):
			return pendingUpdateDebris, nil, nil
		default:
			return pendingUpdateNone, nil, fmt.Errorf("read pending transaction: %w", err)
		}
	}
	if !pendingUpdateSelfDescribing(tx) {
		return pendingUpdateDebris, nil, nil
	}
	if err := validateUpdateTransaction(tx); err != nil {
		if errors.Is(err, errPendingUpdateForeignInstall) {
			return pendingUpdateActionable, nil, nil
		}
		return pendingUpdateNone, nil, fmt.Errorf("validate pending transaction: %w", err)
	}
	return pendingUpdateActionable, tx, nil
}

// isPendingUpdateContentError reports whether err means the bytes on disk are
// not a transaction, as opposed to the file being unreadable. A prepare
// interrupted mid-write leaves a truncated object, which decodes to a syntax
// error rather than an IO error.
func isPendingUpdateContentError(err error) bool {
	var syntax *json.SyntaxError
	var unmarshalType *json.UnmarshalTypeError
	return errors.As(err, &syntax) || errors.As(err, &unmarshalType) || errors.Is(err, io.ErrUnexpectedEOF)
}

// pendingUpdateSelfDescribing reports whether tx carries the identity any
// recovery needs regardless of where Reasonix is installed: which release it
// targets, for which platform, and when it was opened.
func pendingUpdateSelfDescribing(tx *UpdateTransaction) bool {
	if tx == nil || tx.SchemaVersion != updateTransactionVersion || strings.TrimSpace(tx.ToVersion) == "" {
		return false
	}
	if strings.TrimSpace(tx.Platform) == "" || strings.TrimSpace(tx.CreatedAt) == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(tx.CreatedAt))
	return err == nil
}

// quarantinePendingUpdate moves an unusable marker aside and returns where it
// went. It is deliberately not a delete: the marker is the only evidence of
// what went wrong, and a user who reports a stuck updater should still have it.
// Callers hold the pending-update lock.
func quarantinePendingUpdate(reason string) (string, error) {
	path := PendingUpdatePath()
	if path == "" {
		return "", fmt.Errorf("Reasonix state directory is unavailable")
	}
	base := path + ".unusable-" + time.Now().UTC().Format("20060102T150405Z")
	aside := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(aside); os.IsNotExist(err) {
			break
		} else if err != nil {
			return "", err
		}
		aside = fmt.Sprintf("%s-%d", base, i)
	}
	if err := os.Rename(path, aside); err != nil {
		return "", err
	}
	slog.Warn("repair: quarantined an unusable pending update transaction",
		"path", aside, "reason", reason)
	return aside, nil
}

func pendingUpdateMarkerDigest(path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

// quarantinePendingUpdateAfterReconcile rechecks an unusable marker while
// holding the cross-process pending lock. Reconciliation initially classifies
// without that lock because the normal cancel/rollback paths acquire it later;
// the marker digest prevents a concurrent prepare from being quarantined after
// it has replaced the marker.
func quarantinePendingUpdateAfterReconcile(reason string, expectedDigest string, wantForeign bool) (bool, error) {
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return false, fmt.Errorf("lock pending transaction: %w", err)
	}
	defer unlock()

	path := PendingUpdatePath()
	actualDigest, err := pendingUpdateMarkerDigest(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read pending transaction: %w", err)
	}
	if actualDigest != expectedDigest {
		return false, fmt.Errorf("pending update changed while waiting")
	}
	disposition, tx, err := classifyPendingUpdate()
	if err != nil {
		return false, err
	}
	if wantForeign {
		if disposition != pendingUpdateActionable || tx != nil {
			return false, fmt.Errorf("pending update is no longer a foreign transaction")
		}
	} else if disposition != pendingUpdateDebris {
		return false, fmt.Errorf("pending update is no longer unusable debris")
	}
	if _, err := quarantinePendingUpdate(reason); err != nil {
		return false, err
	}
	return true, nil
}

func ReadPendingUpdate() (*UpdateTransaction, error) {
	tx, err := readPendingUpdateUnchecked()
	if err != nil {
		return nil, err
	}
	if err := validateUpdateTransaction(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func readPendingUpdateForLauncher(launcherPath string) (*UpdateTransaction, error) {
	tx, err := readPendingUpdateUnchecked()
	if err != nil {
		return nil, err
	}
	if err := validateUpdateTransactionForLauncher(tx, launcherPath); err != nil {
		return nil, err
	}
	return tx, nil
}

func readPendingUpdateUnchecked() (*UpdateTransaction, error) {
	path := PendingUpdatePath()
	if path == "" {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tx UpdateTransaction
	if err := json.Unmarshal(b, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

func HasPendingUpdate() bool {
	_, err := ReadPendingUpdate()
	return err == nil
}

// PendingUpdateExists reports the on-disk marker even when its contents are
// malformed. It is intended only for progress/UI decisions; callers must use
// ReadPendingUpdate or ReconcilePendingUpdate before authorizing mutations.
func PendingUpdateExists() bool {
	path := PendingUpdatePath()
	if path == "" {
		return false
	}
	_, err := os.Lstat(path)
	return err == nil
}
