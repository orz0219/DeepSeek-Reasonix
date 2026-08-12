package repair

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/filelock"
)

const updateTransactionVersion = 1
const pendingUpdateLockTimeout = 5 * time.Second

var repairExecutable = os.Executable
var updateBackupAfterQuarantine = func(string, string) {}

type UpdateTransaction struct {
	SchemaVersion int    `json:"schemaVersion"`
	FromVersion   string `json:"fromVersion,omitempty"`
	ToVersion     string `json:"toVersion"`
	Platform      string `json:"platform"`
	TargetKind    string `json:"targetKind"` // file | app-bundle
	TargetPath    string `json:"targetPath"`
	BackupPath    string `json:"backupPath"`
	BackupSHA256  string `json:"backupSha256,omitempty"`
	// Files lists every binary of the release unit the update replaces
	// (main executable first, then Guard/launcher siblings). Rollback must
	// restore all of them together: restoring only the main binary would
	// leave a mixed old-desktop/new-Guard install. Empty on transactions
	// recorded by kinds that back up a single unit (macOS app bundles).
	Files     []UpdateTransactionFile `json:"files,omitempty"`
	CreatedAt string                  `json:"createdAt"`
	// Handoff fields authorize the detached macOS updater to act on paths
	// recorded by the live desktop process. They are optional so pending
	// transactions written by older releases remain readable.
	HandoffAppPath       string `json:"handoffAppPath,omitempty"`
	HandoffStagingPath   string `json:"handoffStagingPath,omitempty"`
	HandoffAppTreeID     string `json:"handoffAppTreeId,omitempty"`
	HandoffStagingTreeID string `json:"handoffStagingTreeId,omitempty"`
	HandoffOwnerPID      int    `json:"handoffOwnerPid,omitempty"`
	// BackupTreeID binds a macOS rollback backup to the bundle captured before
	// the update. It remains optional for legacy transactions.
	BackupTreeID string `json:"backupTreeId,omitempty"`
	// OrphanedBackupPath and OrphanedBackupTreeID bind a quarantined backup to
	// the transaction that displaced it. Terminal transaction cleanup removes
	// only this exact tree after re-verifying its digest; older transactions
	// without these optional fields retain their existing behavior.
	OrphanedBackupPath   string `json:"orphanedBackupPath,omitempty"`
	OrphanedBackupTreeID string `json:"orphanedBackupTreeId,omitempty"`
}

type UpdateTransactionFile struct {
	TargetPath       string `json:"targetPath"`
	BackupPath       string `json:"backupPath,omitempty"`
	SHA256           string `json:"sha256,omitempty"`
	InstalledStateID string `json:"installedStateId,omitempty"`
	MissingBefore    bool   `json:"missingBefore,omitempty"`
}

type installedFileUpdateState struct {
	SchemaVersion       int      `json:"schemaVersion"`
	UpdateTransactionID string   `json:"updateTransactionId"`
	InstalledStateIDs   []string `json:"installedStateIds"`
}

// FileUpdateInstallReceipt binds one published release-unit member to the exact
// transaction, target, node type, mode, and bytes that were staged and verified.
// RecordClaimedFileUpdateInstalled accepts only these receipts, so a replacement
// that appears after publish verification cannot be adopted by the transaction.
type FileUpdateInstallReceipt struct {
	UpdateTransactionID string
	TargetPath          string
	InstalledStateID    string
}

type UpdateRollbackResult struct {
	RolledBack  bool   `json:"rolledBack"`
	FromVersion string `json:"fromVersion,omitempty"`
	ToVersion   string `json:"toVersion,omitempty"`
	TargetPath  string `json:"targetPath,omitempty"`
	// MixedInstall reports that a failed rollback could not be compensated:
	// the install now mixes binaries from two releases. Launchers must not
	// start the desktop in this state.
	MixedInstall bool `json:"mixedInstall,omitempty"`
}

// ErrPendingUpdateAwaitingHealth reports that the currently running release is
// still the probationary target of a prior update. Callers must not cancel or
// roll it back merely to start another update; the normal startup health
// confirmation owns that transition.
var ErrPendingUpdateAwaitingHealth = errors.New("previous update is awaiting startup health confirmation")

var errPendingUpdateForeignInstall = errors.New("pending update belongs to a different installation")

// pendingUpdateHealthStaleAfter bounds how long Reconcile waits for startup
// health before auto-committing a still-running probationary target.
var pendingUpdateHealthStaleAfter = 24 * time.Hour

// PendingUpdateReconcileResult describes the safe transition performed before
// startup or a new install. Cleared = pre-publish cancel; RolledBack = verified
// restore; Healthy = probationary target committed after install evidence.
type PendingUpdateReconcileResult struct {
	Pending        bool   `json:"pending"`
	Cleared        bool   `json:"cleared,omitempty"`
	RolledBack     bool   `json:"rolledBack,omitempty"`
	MixedInstall   bool   `json:"mixedInstall,omitempty"`
	AwaitingHealth bool   `json:"awaitingHealth,omitempty"`
	Healthy        bool   `json:"healthy,omitempty"`
	FromVersion    string `json:"fromVersion,omitempty"`
	ToVersion      string `json:"toVersion,omitempty"`
	TargetPath     string `json:"targetPath,omitempty"`
}

// UpdateVersionsEqual reports whether two release version strings name the same
// release, normalizing an optional leading "v"/"V" prefix.
func UpdateVersionsEqual(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return normalizeUpdateVersion(a) == normalizeUpdateVersion(b)
}

func normalizeUpdateVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") && !strings.HasPrefix(v, "V") {
		return "v" + v
	}
	return "v" + strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
}

// pendingUpdateHealthIsStaleOverride forces the stale decision in tests without
// rewriting CreatedAt (part of transaction identity).
var pendingUpdateHealthIsStaleOverride func(*UpdateTransaction) bool

func pendingUpdateHealthIsStale(tx *UpdateTransaction) bool {
	if tx == nil {
		return false
	}
	if pendingUpdateHealthIsStaleOverride != nil {
		return pendingUpdateHealthIsStaleOverride(tx)
	}
	if pendingUpdateHealthStaleAfter <= 0 {
		return false
	}
	created, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(tx.CreatedAt))
	if err != nil {
		created, err = time.Parse(time.RFC3339, strings.TrimSpace(tx.CreatedAt))
	}
	if err != nil {
		return false
	}
	return time.Since(created) >= pendingUpdateHealthStaleAfter
}

// UpdateTransactionID returns a stable, opaque identity for the complete
// transaction. Platform handoff processes use it so copied scalar fields such
// as version and creation time cannot authorize a rewritten pending update.
func UpdateTransactionID(tx *UpdateTransaction) string {
	if tx == nil {
		return ""
	}
	return repairPlanStateID(tx)
}

func PendingUpdatePath() string {
	root := config.MemoryUserDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "repair", "pending-update.json")
}

// lockPendingUpdateStrict serializes cross-process pending-update transitions:
// prepare, rollback, commit, and cancel. Two launchers can run recovery at
// once — a failed update makes startup slow, so a double-clicked Guard is
// realistic — and restoreReleaseUnit's fixed staging/aside paths assume a
// single restorer; unserialized, the loser's compensation can re-install the
// new binaries over the winner's completed rollback.
func lockPendingUpdateStrict() (func(), error) {
	path := PendingUpdatePath()
	if path == "" {
		return nil, fmt.Errorf("pending update: Reasonix state directory is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pendingUpdateLockTimeout)
	defer cancel()
	unlock, err := filelock.Acquire(ctx, path+".lock")
	if err != nil {
		return nil, err
	}
	return unlock, nil
}

var acquirePendingUpdateLock = lockPendingUpdateStrict

// PrepareFileUpdate snapshots the current desktop executable — plus any sibling
// binaries of the release unit the installer also replaces (Guard, launcher,
// update helper) — and records an update transaction before an updater applies
// the replacement. Sibling paths that do not exist are recorded explicitly so
// rollback can remove files introduced by the replacement release.

// Hold the same target locks as rollback so prepare/snapshot cannot race
// a concurrent Guard restore of the release unit.

// PrepareAppBundleUpdate records the sibling bundle backup that the macOS
// handoff script creates. The script performs the directory move after exit.

// PrepareAppBundleUpdateHandoff records every path the detached macOS updater
// may mutate. The child receives only the transaction identity and must claim
// these recorded paths under the pending-update and mutation locks.

// An uncooperative writer is not covered by Reasonix's mutation lock. Recheck
// the public path after quarantine so a recreated node is never adopted as the
// rollback backup of the new transaction.

// ClaimPendingAppBundleUpdateHandoff authorizes a detached child to perform the
// recorded bundle swap. It returns with both the pending transaction lock and
// the target mutation locks held; release must be called on every path.

// ClaimPendingAppBundleUpdateHandoffExact additionally binds the detached
// updater to the complete transaction prepared by the parent process.

// ClaimPendingFileUpdate binds an updater's actual replacement window to the
// exact transaction and release-unit paths prepared by the desktop. The
// launcher path is explicit because the Windows helper runs from a cache
// directory rather than from the installation it is authorized to replace.

// ClaimPendingFileUpdateExact additionally binds the updater to every field in
// the transaction prepared by the desktop process.

// verifyPreparedFileUpdateTargets proves that the release unit still matches
// the exact files snapshotted by PrepareFileUpdate. Path and transaction
// identity alone are insufficient: another installer can replace the binaries
// between prepare and claim while leaving pending-update.json untouched.

// PublishClaimedFileUpdateMember replaces one release-unit member without ever
// overwriting an unverified node. The platform updater must hold the claim
// returned by ClaimPendingFileUpdateExact for the whole release-unit operation.
// A concurrent recreation after the prepared node moves aside wins; the new
// bytes and the verified prior node remain staged for recovery.

// PublishClaimedFileUpdateMemberExact returns proof of the exact node it
// published. Callers must retain every receipt and pass them to
// RecordClaimedFileUpdateInstalled before releasing the update claim.

// RecordClaimedFileUpdateInstalled binds the complete post-install release unit
// while the platform updater still holds the claim's pending and target locks.
// The binding is a transaction-unique create-only sidecar: pending-update.json
// stays immutable, so a process crash can never strand rollback state in the
// gap between displacing the old pending file and publishing a replacement.

// CancelPendingAppBundleUpdateHandoff abandons an exact handoff only when the
// original installed bundle is still the tree captured during prepare. This is
// the safe recovery path when source verification fails after the desktop has
// exited but before any bundle swap occurred.

// CancelPendingAppBundleUpdateHandoffExact abandons only the full transaction
// read or prepared by the caller. It is safe to use after a PID wait or failed
// claim where pending state may have been rewritten with copied scalar IDs.

// VerifyAppBundleUpdateHandoffSource checks the real staging containment and
// the complete staged tree immediately before a handoff mutates the install.
// The lexical metadata check remains readable after staging cleanup, while this
// stronger check is only used while the source bundle still exists.

// CleanupAppBundleUpdateHandoffStaging removes only the complete staging tree
// recorded by the transaction. The root is first displaced to a unique sibling,
// so a concurrent recreation at the public staging path survives.

// CleanupAppBundleUpdateReplacement removes a displaced replacement only when
// its complete tree still matches the transaction's verified source.

// VerifyAppBundleUpdateHandoffTarget proves that the bytes copied into the
// installed bundle are the same tree that was verified in staging.

// VerifyAppBundleUpdateHandoffReplacement proves that a candidate replacement
// tree matches the bundle captured during prepare. The macOS handoff uses this
// before atomically publishing a sibling staging bundle at the install path.

// VerifyAppBundleUpdateHandoffOriginal checks that the installed bundle about
// to become the rollback backup is still the tree captured during prepare.

// VerifyAppBundleUpdateHandoffBackup checks the node produced by the
// target-to-backup rename before the replacement bundle is copied into place.

// quarantineExistingAppBundleUpdateBackup recovers the pre-v1.20 state where
// a committed macOS update removed pending-update.json but its sibling rollback
// bundle survived best-effort cleanup. Without the transaction there is no
// trustworthy authority to delete or reuse that bundle, so preparation moves it
// aside with a no-replace rename and preserves it for diagnosis.
//
// The caller holds both the pending-update lock and the target mutation locks.
// The current executable binding prevents a crafted caller from quarantining a
// similarly named bundle beside an unrelated application.

// cleanupOrphanedAppBundleUpdateBackup retires only the quarantine recorded by
// a terminal transaction. The no-replace move and digest check keep a changed
// or concurrently replaced directory intact for diagnosis instead of deleting
// a path merely because its name resembles a Reasonix quarantine.

// AppBundleTreeDigest exposes the deterministic bundle-content digest to the
// desktop handoff tests and other platform glue without exposing path identity.

// ClearClaimedAppBundleUpdateHandoff removes a failed handoff transaction.
// The caller must still hold the claim returned above.

// WritePendingUpdate is retained for source compatibility with older repair
// callers. Pending transactions are immutable once created; callers that need
// to start an update should use the prepare APIs, and callers that need to
// transition one must use the exact transaction helpers below.
//
// Deprecated: this function only creates a pending transaction and refuses to
// replace an existing one.

// ensureNoPendingUpdate runs with the pending-update lock held. Preparing a new
// transaction over an existing one would overwrite fixed backup paths before
// the new transaction is durable, destroying the previous rollback material if
// preparation later fails.

// Self-describing but not valid for this installation (see
// ReconcilePendingUpdate): nothing can resume or roll it back, and
// refusing here would refuse forever. Quarantine the marker so a
// future update can proceed; target and rollback material are
// untouched.

// Refusing here would be refusing forever: debris cannot be resumed,
// rolled back, or cleared by reconciliation, so every future update
// would fail on a transaction nothing can act on.

// pendingUpdateDisposition is what the pending-update marker on disk currently
// means. Preparation and reconciliation both classify through it so they cannot
// disagree about whether a transaction exists — when they did, a marker that
// reconciliation could not act on still made preparation refuse, and updates
// stayed blocked permanently (#7342).

const (
	// pendingUpdateNone: no marker on disk.
	pendingUpdateNone pendingUpdateDisposition = iota
	// pendingUpdateActionable: a transaction that can still be resumed or
	// rolled back. Preparation must refuse over one of these — writing a new
	// transaction would overwrite fixed backup paths and destroy the rollback
	// material this one still owns.
	pendingUpdateActionable
	// pendingUpdateDebris: a marker that cannot describe a recoverable
	// transaction, so it owns no rollback material worth protecting.
	pendingUpdateDebris
)

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

// isPendingUpdateContentError reports whether err means the bytes on disk are
// not a transaction, as opposed to the file being unreadable. A prepare
// interrupted mid-write leaves a truncated object, which decodes to a syntax
// error rather than an IO error.

// pendingUpdateSelfDescribing reports whether tx carries the identity any
// recovery needs regardless of where Reasonix is installed: which release it
// targets, for which platform, and when it was opened.

// quarantinePendingUpdate moves an unusable marker aside and returns where it
// went. It is deliberately not a delete: the marker is the only evidence of
// what went wrong, and a user who reports a stuck updater should still have it.
// Callers hold the pending-update lock.

// quarantinePendingUpdateAfterReconcile rechecks an unusable marker while
// holding the cross-process pending lock. Reconciliation initially classifies
// without that lock because the normal cancel/rollback paths acquire it later;
// the marker digest prevents a concurrent prepare from being quarantined after
// it has replaced the marker.

// PendingUpdateExists reports the on-disk marker even when its contents are
// malformed. It is intended only for progress/UI decisions; callers must use
// ReadPendingUpdate or ReconcilePendingUpdate before authorizing mutations.

// ReconcilePendingUpdate resolves an older immutable update transaction before
// startup or a new install. It first attempts the narrow cancellation path,
// which succeeds only while every original target still matches the state
// captured by prepare and no replacement state is durable. If publication has
// started, it falls back to the exact verified rollback path. Both transitions
// re-read the complete transaction under the pending and target mutation locks.
//
// A transaction targeting runningVersion is left untouched only when the
// transaction also proves that its replacement release unit is installed and
// its rollback state is intact. Version equality alone is not installation
// evidence: a same-version/manual launch may observe an abandoned prepare.

// Nothing here can be resumed or rolled back. Leaving it in place is
// what stranded users: startup kept failing to recover it while
// preparation kept refusing to write over it.

// Self-describing but not valid for this installation: the launcher and
// target directories no longer match (install layout moved to
// versions\<version>\ or the old install directory is gone). Nothing
// here can be resumed or rolled back by this installation, and leaving
// the marker blocks every future update permanently — recovery fails
// here before preparation can act, and the marker lives in the state
// directory so a reinstall does not clear it (#7391, #7416, #7407).
// Quarantine the marker, never a delete: the target and rollback
// material are untouched, so a genuine transaction observed from the
// wrong install loses nothing and remains recoverable from the
// .unusable-* file by hand.

// After the stale window, auto-commit a still-running probationary target.

// Cancel is deliberately attempted before rollback. For app bundles it
// requires the original tree and an absent backup; for file release units it
// requires every prepared target and no installed-state sidecar. A failed
// cancel never mutates the transaction or release unit.

// Exact cancellation historically treats a different target version or
// creation time as an inert success. Re-check the public postcondition so
// reconciliation never reports a newer transaction as cleared.

// Another exact owner may have committed or cancelled the transaction
// between the invocation snapshot and the locked transition.

// CommitProbationaryPendingUpdate commits a still-running probationary update
// when install evidence matches. Returns true when the marker is gone.

// AbandonPendingUpdate is the user-initiated recovery path for a stuck
// transaction: commit if possible, else reconcile, else force-retire.

// Keep going: a drifted backup must not block explicit discard.

// Force-retire when still AwaitingHealth with the live target installed.

// forceRetireProbationaryPendingUpdate retires a probationary marker when the
// live target is installed but MarkUpdateHealthy cannot finish (e.g. bad backup).

// Target-only evidence: broken rollback backups must not block discard.

// pendingUpdateTargetInstalled reports live replacement install evidence only
// (no rollback backup requirement). Used by force-retire on explicit abandon.

// pendingUpdateInstalledForHealth requires transaction-bound evidence for the
// complete replacement and rollback unit. It intentionally treats missing or
// drifted evidence as uninstalled so reconciliation can take the existing
// exact cancel/rollback paths instead of trusting a version string.

// cleanupPendingUpdateStaging is best-effort after the pending transaction has
// been safely committed away. CleanupAppBundleUpdateHandoffStaging verifies the
// complete recorded tree before removal, so drifted or recreated paths survive.

// MarkUpdateHealthy commits a probationary update and removes its backup. A
// version mismatch is ignored so an older process cannot bless a newer update.

// MarkUpdateHealthyMatching commits only the exact pending transaction observed
// when this desktop process started. The creation identity prevents an older
// process from blessing a later same-version retry.

// MarkUpdateHealthyExact commits only the complete transaction captured before
// the replacement process started.

// CancelPendingUpdate removes a transaction that failed before control was
// handed to the replacement build. A version mismatch is intentionally inert.

// CancelPendingUpdateMatching removes only the exact transaction prepared by
// the caller. It is used by updater failure paths where a same-version retry can
// replace pending-update.json before cleanup runs.

// CancelPendingUpdateExact removes only the complete transaction returned by
// prepare. This is the updater failure path: copied creation timestamps are not
// sufficient authorization if pending-update.json itself was rewritten.

// RollbackPendingUpdateMatching rolls back only the exact transaction prepared
// by the caller. This is used when an apply attempt fails after another process
// may already have replaced pending-update.json with a same-version retry.

// RollbackPendingUpdateExact restores only the complete transaction returned by
// prepare. It is used after a platform apply failure where a later same-version
// transaction must remain untouched.

// The expected-match checks below re-run under the strict lock, so a
// transaction committed, cancelled, or replaced while waiting here is never
// acted upon.

// rollbackPendingUpdateMatchingLocked performs the transition while the caller
// holds the pending-update lock. RecoverFailedInstall uses this form so failure
// marker correlation, rollback, and marker cleanup are one serialized state
// transition.

// Share release-unit target locks with other repair mutations so two
// REASONIX_HOME profiles cannot quarantine or restore the same binaries
// through different pending-update locks.

// Invocation-local binding proves that the live unit did not drift while
// this rollback waited for locks. It does not prove that an unbound live
// node belongs to the pending transaction and therefore cannot authorize
// deleting the retained aside after restore.

// Verify every backup before touching any binary: a partial restore
// would recreate exactly the mixed-version install rollback exists to
// prevent. A missing hash is a validation failure, not a bypass —
// ReadPendingUpdate already rejects hashless file transactions, so
// this guards hand-crafted callers.

// Rename/copy indirection so tests can inject mid-unit failures.
var (
	rollbackStageCopy          = copyFileWithHashCreate
	rollbackPublishStage       = renameRepairNodeNoReplace
	rollbackSwapRename         = renameRepairNodeNoReplace
	removePendingUpdateFile    = os.Remove
	pendingUpdateBeforeCleanup = func(string) {}
	updateCleanupAfterRename   = func(string, string) {}
	fileUpdateAfterRetain      = func(string, string) {}
	installedUpdateAfterCreate = func(string) {}
)

// restoreReleaseUnit swaps every backup into place with compensation, so a
// failed rollback never leaves a mixed old/new install. Phase 1 stages each
// backup next to its target — a copy can fail halfway (disk full, unreadable
// backup) and staging keeps the live binaries untouched until every byte is
// on the target filesystem. Phase 2 swaps via renames only: each target moves
// aside first (renaming works even for the running executable, where
// overwriting does not), so a failure renames the asides back and the unit
// stays coherent on the new version for a retried rollback. Only when that
// unwinding itself fails is the install reported as mixed.

// The backup can change after the preflight hash but before or during
// this copy. Bind the bytes that will actually be installed, not only
// the source path observed before staging.

// A crash can leave an old-version target beside the retained new-version
// aside after the stage-to-target rename. Recognize that exact state before
// touching any entry so a retry preserves the aside for compensation instead
// of overwriting it with the already-restored target.

// A rollback interrupted between renames may have consumed this
// target while retaining the new binary at the fixed aside path.
// Preserve that copy for compensation until the retry succeeds.

// The old release did not contain this path. Retaining the new file
// at the aside path removes it from the live release atomically; it
// is deleted only after the whole rollback succeeds.

// Stage and target share a filesystem. A no-replace rename publishes the
// fully verified bytes atomically, consumes the writable staging alias,
// and refuses to overwrite a target recreated after the confirmed node
// moved aside.

// Best-effort: on Windows the running executable's aside may linger
// until the process exits, but it is no longer a live entry point.

// Compensate: rename the new-version binaries back over the restored old
// ones. A missing-before entry is compensated the same way: move the
// retained new file back to its original path.

// An aside inherited from a crashed process has no durable content
// binding. Never move it back into an executable path during
// compensation; leave recovery material in place and fail closed.

// Atomically displace and verify only bytes this rollback
// published. Anything else is restored or retained.

// No retained new-version copy exists to put back after the old
// backup was (or may have been) placed.

// allowedUpdateTargetBase whitelists the packaged binaries an update
// transaction may name. The main executable names are only valid as the
// primary target; Guard/launcher artifacts only as release-unit siblings.

// Every restorable file must carry a hash — rollback promises to
// verify all backups before touching any binary, so an unhashed entry
// would silently weaken that gate.
