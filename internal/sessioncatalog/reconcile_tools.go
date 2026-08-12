package sessioncatalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/projectiondb"
)

func directorySignature(dir string) (string, error) {

	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing", nil
		}
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".meta")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%d\n", name, info.Size(), info.ModTime().UnixNano(), info.Mode())
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func recordFromOrder(target DirectoryTarget, info agent.SessionOrderInfo) SessionRecord {
	scope, root := normalizeScope(info.Scope, info.WorkspaceRoot)
	if info.TopicID == "" {
		scope, root = target.Scope, target.WorkspaceRoot
	}
	turnsState := TurnsValid
	if info.SchemaVersion < agent.BranchMetaCountsVersion && info.Turns == 0 {
		turnsState = TurnsUnknown
	}
	contentFingerprint := fileFingerprint(info.Path)
	metaFingerprint := fileFingerprint(agent.BranchMetaPath(info.Path))
	createdAt := unixMilli(info.CreatedAt)
	lastActivityAt := unixMilli(info.LastActivityAt)

	if st, err := os.Stat(info.Path); err == nil {
		fileMS := st.ModTime().UnixMilli()
		if createdAt <= 0 {
			createdAt = fileMS
		}
		if lastActivityAt <= 0 || fileMS > lastActivityAt {
			lastActivityAt = fileMS
		}
	}

	recoveryCopy := info.Recovered && agent.RecoveryBranchCoveredByParent(info.Path, target.Path)
	return normalizeSessionRecord(SessionRecord{
		Path:               info.Path,
		Directory:          target.Path,
		Scope:              scope,
		WorkspaceRoot:      root,
		TopicID:            info.TopicID,
		TopicTitle:         info.TopicTitle,
		CustomTitle:        info.CustomTitle,
		CreatedAt:          createdAt,
		LastActivityAt:     lastActivityAt,
		Preview:            info.Preview,
		Turns:              info.Turns,
		TurnsState:         turnsState,
		Recovered:          info.Recovered,
		RecoveryReason:     info.RecoveryReason,
		RecoveryDigest:     info.RecoveryDigest,
		ParentID:           info.ParentID,
		RecoveryPreferred:  info.RecoveryPreferred,
		RecoveryCopy:       recoveryCopy,
		ContentFingerprint: contentFingerprint,
		MetaFingerprint:    metaFingerprint,
		Health:             HealthOK,
	})
}

func unixMilli(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixMilli()
}

func fileFingerprint(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// Rebuild replaces only the disposable catalog. Authoritative sessions and
// sidecars are never changed or removed by this operation. The live database
// stays in place until a fully-populated replacement is validated and swapped.
func Rebuild(ctx context.Context, path string, targets []DirectoryTarget) (Status, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath()
	}
	if strings.TrimSpace(path) == "" {
		catalog, err := Open(ctx, Options{InMemory: true, DisableRepair: true})
		if err != nil {
			return Status{}, err
		}
		for _, target := range targets {
			if err := catalog.ReconcileDirectory(ctx, target); err != nil {
				closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = catalog.Close(closeCtx)
				cancel()
				return catalog.Status(), err
			}
		}
		status := catalog.Status()
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = catalog.Close(closeCtx)
		cancel()
		return status, nil
	}
	err := projectiondb.Rebuild(ctx, projectiondb.OpenOptions{
		Path:       path,
		MemoryName: "session-catalog-rebuild",
		Migrations: sessionMigrations(),
	}, func(ctx context.Context, db *sql.DB) error {

		temp := &Catalog{
			db:             db,
			opts:           Options{Path: path, DisableRepair: true, Now: time.Now, MissingGrace: defaultMissingGrace},
			writeQueued:    map[string]SessionRecord{},
			directoryLocks: map[string]*sync.Mutex{},
			stop:           make(chan struct{}),
			status:         Status{State: StateReady, Mode: ModeDisk, Path: path},
		}
		temp.workerCtx, temp.workerCancel = context.WithCancel(ctx)
		defer temp.workerCancel()
		for _, target := range targets {
			if err := temp.ReconcileDirectory(ctx, target); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Status{}, err
	}

	catalog, err := Open(ctx, Options{Path: path, DisableRepair: true})
	if err != nil {
		return Status{State: StateReady, Mode: ModeDisk, Path: path}, nil
	}
	status := catalog.Status()
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = catalog.Close(closeCtx)
	cancel()
	return status, nil
}

// Inspect is read-only. It never migrates, repairs, quarantines, or rewrites a
// catalog, making it suitable for `reasonix doctor sessions`.
func Inspect(ctx context.Context, path string) (Status, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultPath()
	}
	status := Status{State: StateDegraded, Mode: ModeDisk, Path: path}
	if strings.TrimSpace(path) == "" {
		status.LastError = "catalog path unavailable"
		return status, nil
	}
	inspection := projectiondb.Inspect(ctx, path)
	if inspection.Error != "" && !inspection.Exists {
		status.LastError = inspection.Error
		return status, nil
	}
	if !inspection.Exists {
		status.LastError = "catalog does not exist"
		return status, nil
	}
	if inspection.Integrity != "" && inspection.Integrity != "ok" {
		status.LastError = inspection.Integrity
		return status, nil
	}
	if inspection.Error != "" {
		status.LastError = inspection.Error
		return status, nil
	}
	u := &url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout%28150%29")
	if err != nil {
		return status, err
	}
	defer db.Close()
	status.State = StateReady
	_ = db.QueryRowContext(ctx, `SELECT revision FROM catalog_state WHERE id=1`).Scan(&status.Revision)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_sessions`).Scan(&status.Indexed)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_sessions WHERE turns_state='unknown'`).Scan(&status.RepairPending)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_sessions WHERE missing_since=0`).Scan(&status.PhysicalSessions)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_topics`).Scan(&status.LogicalSessions)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT recovery_group_id) FROM catalog_sessions WHERE recovered=1 AND recovery_group_id<>'' AND missing_since=0`).Scan(&status.RecoveryGroups)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_sessions WHERE recovered=1 AND missing_since=0`).Scan(&status.RecoveryBranches)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_sessions WHERE recovered=1 AND recovery_role='diverged' AND missing_since=0`).Scan(&status.RecoveryDiverged)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_sessions WHERE recovered=1 AND recovery_role='covered_copy' AND missing_since=0`).Scan(&status.CleanupEligible)
	return status, nil
}
