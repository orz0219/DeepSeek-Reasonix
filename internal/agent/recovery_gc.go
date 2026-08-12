package agent

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/provider"
)

// Recovery-branch garbage collection. Conflict recovery forks a copy of the
// in-memory transcript whenever a save conflicts (#5993); the triggers are
// fixed, but every fork that ever happened still sits in the session list
// until the user trashes it by hand. Most of them preserve nothing: the
// original session went on to contain everything the fork saved. Those — and
// only those — are safe to reclaim automatically.

// RecoveryGCGracePeriod is how long a reclaimable recovery branch must sit
// idle before the periodic GC may collect it. A fresh fork is part of an
// active conflict flow — the user may be comparing it against the original.
const RecoveryGCGracePeriod = 24 * time.Hour

// RecoveryGCStartupGracePeriod is used for the first post-restore sweep so
// upgrade storms of covered copies are cleared within minutes rather than a
// full day, while still protecting a conflict the user is actively inspecting.
const RecoveryGCStartupGracePeriod = 15 * time.Minute

// ErrRecoveryBranchNotCovered means the branch cannot currently be proven
// redundant with its parent. Destructive callers must preserve it.
var ErrRecoveryBranchNotCovered = errors.New("recovery branch is not covered by its parent")

// ErrRecoveryBranchNotIdle means the branch has not yet passed the safety
// grace period. It remains visible and may be retried by a later GC pass.
var ErrRecoveryBranchNotIdle = errors.New("recovery branch is still inside its safety grace period")

type recoveryTrashMeta struct {
	Key       string `json:"key"`
	DeletedAt int64  `json:"deletedAt"`
}

type recoveryTrashPendingMeta struct {
	Key string `json:"key"`
}

// SessionLeaseHeld reports whether ANY live runtime — this process included —
// holds the session's write lease. SessionLeaseHeldByOtherRuntime deliberately
// answers false for the current process; GC needs the stricter question, since
// a branch open in one of our own tabs is just as much in use.
func SessionLeaseHeld(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	if _, ok := sessionLeaseOwners.Load(canonicalSessionSavePath(path)); ok {
		return true
	}
	return SessionLeaseHeldByOtherRuntime(path)
}

// RecoveryBranchCoveredByParent reports whether a conflict-recovery branch
// preserves no content that is absent from its parent. It deliberately reads
// both transcripts instead of trusting listing sidecars: stale metadata must
// never authorize hiding, migration skipping, bulk trash, or permanent purge.
// Missing/corrupt metadata, a changed branch, or a missing/diverged parent are
// all treated conservatively as not covered.
func RecoveryBranchCoveredByParent(path, parentDir string) bool {
	meta, ok, err := LoadBranchMeta(path)
	if err != nil || !ok || !meta.Recovered || strings.TrimSpace(meta.RecoveryDigest) == "" {
		return false
	}
	return recoveryBranchCoveredByParent(path, parentDir, meta)
}

// SessionContentCovers reports whether covering contains the complete message
// history stored in covered. It is intentionally conservative for catalog
// canonical promotion: repaired or damaged loads cannot authorize a redirect.
func SessionContentCovers(coveringPath, coveredPath string) bool {
	covering, ok := LoadSessionContentSnapshot(coveringPath)
	if !ok {
		return false
	}
	covered, ok := LoadSessionContentSnapshot(coveredPath)
	return ok && covering.Covers(covered)
}

// SetRecoveryPreferred records exactly one explicit preferred leaf. It clears
// old choices first, so interruption can only fall back to an unresolved group;
// it can never leave two canonical choices.
func SetRecoveryPreferred(paths []string, chosenPath string) error {
	chosenPath = canonicalSessionSavePath(chosenPath)
	if chosenPath == "" {
		return fmt.Errorf("empty preferred recovery path")
	}
	unique := map[string]struct{}{}
	for _, path := range paths {
		path = canonicalSessionSavePath(path)
		if path != "" {
			unique[path] = struct{}{}
		}
	}
	if _, ok := unique[chosenPath]; !ok {
		return fmt.Errorf("preferred recovery path is outside the lineage")
	}
	ordered := make([]string, 0, len(unique))
	for path := range unique {
		meta, ok, err := LoadBranchMeta(path)
		if err != nil || !ok || !meta.Recovered {
			return fmt.Errorf("invalid recovery lineage member")
		}
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, path := range ordered {
		if err := UpdateBranchMeta(path, false, func(meta *BranchMeta) error {
			meta.RecoveryPreferred = false
			meta.RecoveryPreferredDigest = ""
			return nil
		}); err != nil {
			return err
		}
	}
	chosen, err := LoadSession(chosenPath)
	if err != nil || chosen == nil || chosen.normalizedDirty || chosen.eventLogDamaged {
		return fmt.Errorf("could not fingerprint preferred recovery branch")
	}
	digest, err := digestSessionMessages(chosen.Snapshot())
	if err != nil {
		return err
	}
	return UpdateBranchMeta(chosenPath, false, func(meta *BranchMeta) error {
		if !meta.Recovered {
			return fmt.Errorf("preferred session is not a recovery branch")
		}
		meta.RecoveryPreferred = true
		meta.RecoveryPreferredDigest = digestString(digest)
		return nil
	})
}

// RecoveryPreferenceCurrent proves that the branch still has the exact content
// the user selected. Continued or externally edited branches fall back to an
// unresolved lineage until the user chooses again.
func RecoveryPreferenceCurrent(path string, meta BranchMeta) bool {
	if !meta.RecoveryPreferred || strings.TrimSpace(meta.RecoveryPreferredDigest) == "" {
		return false
	}
	session, err := LoadSession(path)
	if err != nil || session == nil || session.normalizedDirty || session.eventLogDamaged {
		return false
	}
	digest, err := digestSessionMessages(session.Snapshot())
	return err == nil && digestString(digest) == strings.TrimSpace(meta.RecoveryPreferredDigest)
}

// SessionContentSnapshot is an immutable, validated transcript projection used
// by the catalog. Its messages stay private so callers cannot accidentally use
// a classification read as a mutable Session.
type SessionContentSnapshot struct {
	messages []provider.Message
}

// LoadSessionContentSnapshot loads one transcript once for lineage analysis.
// Dirty normalization and damaged event logs fail closed: neither may prove a
// canonical branch or authorize cleanup.
func LoadSessionContentSnapshot(path string) (SessionContentSnapshot, bool) {
	session, err := LoadSession(path)
	if err != nil || session == nil || session.normalizedDirty || session.eventLogDamaged {
		return SessionContentSnapshot{}, false
	}
	return SessionContentSnapshot{messages: session.Snapshot()}, true
}

// Len is used only to discard candidates that cannot cover the longest member.
func (s SessionContentSnapshot) Len() int { return len(s.messages) }

// Covers reports whether s contains all content in covered as a compatible
// prefix. Both snapshots have already passed the conservative load checks.
func (s SessionContentSnapshot) Covers(covered SessionContentSnapshot) bool {
	return messagesHavePrefix(s.messages, covered.messages) ||
		messagesHavePrefixWithCompatibleSystem(s.messages, covered.messages)
}

// TryAcquireRecoveryParentGuard verifies that a recovery branch is covered by
// its parent while holding the parent's save and lease locks. The caller must
// keep the returned guard until permanent deletion finishes, then Release it.
// If the parent is open or being rewritten, acquisition fails without waiting
// so bulk cleanup preserves the branch and can be retried later.
func TryAcquireRecoveryParentGuard(path, parentDir string) (*SessionRemovalGuard, error) {
	meta, ok, err := LoadBranchMeta(path)
	if err != nil || !ok || !meta.Recovered || strings.TrimSpace(meta.RecoveryDigest) == "" {
		return nil, ErrRecoveryBranchNotCovered
	}
	parentID := strings.TrimSpace(meta.ParentID)
	if parentID == "" {
		return nil, ErrRecoveryBranchNotCovered
	}
	parentDir = strings.TrimSpace(parentDir)
	if parentDir == "" {
		parentDir = filepath.Dir(path)
	}
	parentPath := filepath.Join(parentDir, parentID+".jsonl")
	if parentPath == path || !IsVisibleSession(parentPath) {
		return nil, ErrRecoveryBranchNotCovered
	}
	guard, err := TryAcquireSessionRemovalGuard(parentPath)
	if err != nil {
		return nil, err
	}
	if !recoveryBranchCoveredByParent(path, parentDir, meta) {
		guard.Release()
		return nil, ErrRecoveryBranchNotCovered
	}
	return guard, nil
}

func recoveryBranchCoveredByParent(path, parentDir string, meta BranchMeta) bool {
	parentID := strings.TrimSpace(meta.ParentID)
	if parentID == "" {
		return false
	}
	branch, err := LoadSession(path)
	if err != nil || branch == nil {
		return false
	}
	branchMsgs := branch.Snapshot()
	branchDigest, err := digestSessionMessages(branchMsgs)
	if err != nil || digestString(branchDigest) != strings.TrimSpace(meta.RecoveryDigest) {
		// Continued on (or undigestable): this is someone's conversation now.
		return false
	}
	parentDir = strings.TrimSpace(parentDir)
	if parentDir == "" {
		parentDir = filepath.Dir(path)
	}
	parentPath := filepath.Join(parentDir, parentID+".jsonl")
	if parentPath == path || !IsVisibleSession(parentPath) {
		return false
	}
	parent, err := LoadSession(parentPath)
	if err != nil || parent == nil {
		return false
	}
	parentMsgs := parent.Snapshot()
	parentDigest, err := digestSessionMessages(parentMsgs)
	if err != nil {
		return false
	}
	return bytes.Equal(parentDigest[:], branchDigest[:]) ||
		messagesHavePrefix(parentMsgs, branchMsgs) ||
		messagesHavePrefixWithCompatibleSystem(parentMsgs, branchMsgs)
}

// ReclaimableRecoveryBranches scans dir for conflict-recovery branches that
// are safe to dispose of. Every condition must hold — when in doubt the branch
// stays, because a recovery branch exists precisely to prevent data loss:
//
//  1. The branch meta says Recovered and records the fork digest.
//  2. The transcript still matches that fork digest: the branch was never
//     continued on. A single follow-up turn disqualifies it permanently.
//  3. The parent transcript (meta.ParentID, same directory) exists and covers
//     the branch content — equal digest, or the branch is a strict prefix
//     (allowing a compatible leading-system swap). These are the same checks
//     SaveRecoveryBranch uses to declare a recovery not needed in the first
//     place, so "covered" here means the fork preserves nothing unique.
//  4. No live runtime holds the branch's session lease.
//  5. The branch has been idle for at least grace.
//
// It returns candidate paths only; disposal (trash, delete) is caller policy.
func ReclaimableRecoveryBranches(dir string, now time.Time, grace time.Duration) ([]string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") || strings.HasSuffix(e.Name(), ".events.jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if !IsVisibleSession(path) {
			continue
		}
		meta, ok, err := LoadBranchMeta(path)
		if err != nil || !ok || !meta.Recovered || strings.TrimSpace(meta.RecoveryDigest) == "" {
			continue
		}
		if strings.TrimSpace(meta.ParentID) == "" {
			continue
		}
		if !recoveryBranchIdle(path, meta, now, grace) {
			continue
		}
		if SessionLeaseHeld(path) {
			continue
		}
		if !recoveryBranchCoveredByParent(path, dir, meta) {
			continue
		}
		out = append(out, path)
	}
	return out, nil
}

// TrashCoveredRecoveryBranch moves a redundant recovery branch into the same
// recoverable .trash layout used by Desktop. This is the explicit/manual cleanup
// path, so it does not require the background GC idle grace period. Parent
// coverage is rechecked while both parent and branch removal guards are held.
func TrashCoveredRecoveryBranch(path, parentDir string) error {
	return trashCoveredRecoveryBranch(path, parentDir, false)
}

// TrashRecoveryBranchCoveredBy moves path to recoverable trash when canonical
// contains its complete transcript. Unlike TrashCoveredRecoveryBranch this is
// lineage-aware: legacy recovery storms form chains, so an adopted leaf may
// cover an ancestor even though that ancestor's immediate parent does not cover
// it. Both transcripts are held behind removal guards while coverage is proved.
func TrashRecoveryBranchCoveredBy(path, canonicalPath, parentDir string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	canonicalPath = filepath.Clean(strings.TrimSpace(canonicalPath))
	parentDir = filepath.Clean(strings.TrimSpace(parentDir))
	if path == "." || canonicalPath == "." || parentDir == "." || path == canonicalPath ||
		filepath.Dir(path) != parentDir || filepath.Dir(canonicalPath) != parentDir {
		return ErrRecoveryBranchNotCovered
	}
	meta, ok, err := LoadBranchMeta(path)
	if err != nil || !ok || !meta.Recovered {
		return ErrRecoveryBranchNotCovered
	}
	paths := []string{path, canonicalPath}
	sort.Strings(paths)
	guards := make(map[string]*SessionRemovalGuard, len(paths))
	for _, guardedPath := range paths {
		guard, guardErr := TryAcquireSessionRemovalGuard(guardedPath)
		if guardErr != nil {
			for _, held := range guards {
				held.Release()
			}
			return guardErr
		}
		guards[guardedPath] = guard
	}
	defer func() {
		for _, guard := range guards {
			guard.Release()
		}
	}()
	if !SessionContentCovers(canonicalPath, path) {
		return ErrRecoveryBranchNotCovered
	}
	key := filepath.Base(path)
	if !validRecoveryTrashKey(key) {
		return fmt.Errorf("invalid recovery session path")
	}
	stageDir, err := reserveRecoveryTrashStage(parentDir)
	if err != nil {
		return err
	}
	if err := prepareRecoveryTrashStage(path, key, stageDir); err != nil {
		return err
	}
	return finishRecoveryTrashStage(parentDir, path, key, stageDir, guards[path])
}

// ReparentRecoveryCanonical shortens a proved recovery chain to root ->
// canonical without rewriting either transcript. This preserves discovery for
// older versions after covered intermediate branches are moved to trash.
func ReparentRecoveryCanonical(canonicalPath, rootID, parentDir string) error {
	canonicalPath = filepath.Clean(strings.TrimSpace(canonicalPath))
	parentDir = filepath.Clean(strings.TrimSpace(parentDir))
	rootID = strings.TrimSpace(rootID)
	rootPath := filepath.Join(parentDir, rootID+".jsonl")
	if canonicalPath == "." || parentDir == "." || rootID == "" || filepath.Base(rootID) != rootID ||
		filepath.Dir(canonicalPath) != parentDir || canonicalPath == rootPath {
		return ErrRecoveryBranchNotCovered
	}
	paths := []string{canonicalPath, rootPath}
	sort.Strings(paths)
	guards := make([]*SessionRemovalGuard, 0, len(paths))
	for _, path := range paths {
		guard, err := TryAcquireSessionRemovalGuard(path)
		if err != nil {
			for _, held := range guards {
				held.Release()
			}
			return err
		}
		guards = append(guards, guard)
	}
	defer func() {
		for _, guard := range guards {
			guard.Release()
		}
	}()
	if !SessionContentCovers(canonicalPath, rootPath) {
		return ErrRecoveryBranchNotCovered
	}
	return UpdateBranchMeta(canonicalPath, false, func(meta *BranchMeta) error {
		if !meta.Recovered {
			return ErrRecoveryBranchNotCovered
		}
		meta.ParentID = rootID
		meta.RecoveryDepth = 1
		return nil
	})
}

// TrashReclaimableRecoveryBranch is the background-GC variant. In addition to
// the same atomic coverage proof, it requires the branch to remain idle for the
// full grace period.
func TrashReclaimableRecoveryBranch(path, parentDir string) error {
	return trashCoveredRecoveryBranch(path, parentDir, true)
}

func trashCoveredRecoveryBranch(path, parentDir string, requireIdle bool) error {
	path = filepath.Clean(strings.TrimSpace(path))
	parentDir = filepath.Clean(strings.TrimSpace(parentDir))
	if path == "." || parentDir == "." || filepath.Dir(path) != parentDir {
		return fmt.Errorf("recovery branch must be a direct child of its session directory")
	}
	key := filepath.Base(path)
	if !strings.HasSuffix(key, ".jsonl") || strings.HasSuffix(key, ".events.jsonl") {
		return fmt.Errorf("invalid recovery session path")
	}

	parentGuard, err := TryAcquireRecoveryParentGuard(path, parentDir)
	if err != nil {
		return err
	}
	defer parentGuard.Release()

	branchGuard, err := TryAcquireSessionRemovalGuard(path)
	if err != nil {
		return err
	}
	defer branchGuard.Release()
	if requireIdle {
		meta, ok, err := LoadBranchMeta(path)
		if err != nil || !ok || !recoveryBranchIdle(path, meta, time.Now(), RecoveryGCGracePeriod) {
			return ErrRecoveryBranchNotIdle
		}
	}
	if !RecoveryBranchCoveredByParent(path, parentDir) {
		return ErrRecoveryBranchNotCovered
	}

	stageDir, err := reserveRecoveryTrashStage(parentDir)
	if err != nil {
		return err
	}
	// Keep the move invisible until every artifact is staged. Older Reasonix
	// versions ignore the non-session staging directory, while new versions can
	// finish it from the durable in-directory marker after a crash. Publishing is
	// one same-filesystem rename, so Desktop can never restore or purge a split
	// transcript/sidecar set.
	if err := prepareRecoveryTrashStage(path, key, stageDir); err != nil {
		return err
	}
	return finishRecoveryTrashStage(parentDir, path, key, stageDir, branchGuard)
}

func recoveryBranchIdle(path string, meta BranchMeta, now time.Time, grace time.Duration) bool {
	idleSince := meta.UpdatedAt
	if idleSince.IsZero() {
		info, err := os.Stat(path)
		if err != nil {
			return false
		}
		idleSince = info.ModTime()
	}
	return now.Sub(idleSince) >= grace
}

// reconcileRecoveryTrashPending completes an interrupted move left by the
// pre-staging recovery-trash protocol. Keep this compatibility path so users
// upgrading from an intermediate build do not strand its typed marker.

// reconcileRecoveryTrashStages completes the atomic staging protocol used by
// new runtimes. A staging directory is intentionally not a valid Desktop trash
// item: it has neither a session-shaped directory name nor .trash-meta.json.
// Once complete, the whole directory is renamed into place atomically.

// A non-staging name means the atomic directory rename already succeeded;
// only visibility metadata/final marker cleanup may remain.

// restored or purged after complete publication

// The durable marker landed but the first rename did not. No session
// artifact has moved, so discard the empty stage and let a later GC pass
// revalidate coverage before trying again.

// A complete entry may be restored or purged as soon as it is published.
// In that case there is nothing left for this producer to finalize.

// another producer won this candidate

// Keep the branch guard until the trash entry is complete and the hidden
// marker is cleared. No runtime can bind the now-vacant live path in the
// middle and inherit an incomplete cleanup state.
