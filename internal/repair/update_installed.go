package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/fileutil"
)

// RecordClaimedFileUpdateInstalled binds the complete post-install release unit
// while the platform updater still holds the claim's pending and target locks.
// The binding is a transaction-unique create-only sidecar: pending-update.json
// stays immutable, so a process crash can never strand rollback state in the
// gap between displacing the old pending file and publishing a replacement.
func RecordClaimedFileUpdateInstalled(
	claimed *UpdateTransaction,
	receipts ...FileUpdateInstallReceipt,
) (*UpdateTransaction, error) {
	if claimed == nil || claimed.TargetKind != "file" {
		return nil, fmt.Errorf("record installed update: transaction identity is incomplete")
	}
	current, err := readPendingUpdateForLauncher(claimed.TargetPath)
	if err != nil {
		return nil, fmt.Errorf("record installed update: read pending transaction: %w", err)
	}
	if !reflect.DeepEqual(claimed, current) {
		return nil, fmt.Errorf("record installed update: pending transaction changed")
	}
	if len(current.Files) == 0 {
		return nil, fmt.Errorf("record installed update: release unit is incomplete")
	}
	record := &installedFileUpdateState{
		SchemaVersion:       1,
		UpdateTransactionID: UpdateTransactionID(current),
		InstalledStateIDs:   make([]string, len(current.Files)),
	}
	receiptStates := make(map[string]string, len(receipts))
	for _, receipt := range receipts {
		if strings.TrimSpace(receipt.UpdateTransactionID) != record.UpdateTransactionID {
			return nil, fmt.Errorf("record installed update: publish receipt belongs to a different transaction")
		}
		targetKey := canonicalRepairPath(receipt.TargetPath)
		if targetKey == "" {
			return nil, fmt.Errorf("record installed update: publish receipt target is invalid")
		}
		stateID := strings.TrimSpace(receipt.InstalledStateID)
		if len(stateID) != sha256.Size*2 {
			return nil, fmt.Errorf("record installed update: publish receipt state is invalid")
		}
		if _, err := hex.DecodeString(stateID); err != nil {
			return nil, fmt.Errorf("record installed update: publish receipt state is invalid")
		}
		if _, exists := receiptStates[targetKey]; exists {
			return nil, fmt.Errorf("record installed update: duplicate publish receipt")
		}
		receiptStates[targetKey] = stateID
	}
	for i := range current.Files {
		f := &current.Files[i]
		targetKey := canonicalRepairPath(f.TargetPath)
		if stateID, ok := receiptStates[targetKey]; ok {
			record.InstalledStateIDs[i] = stateID
			delete(receiptStates, targetKey)
			continue
		}
		info, statErr := os.Lstat(f.TargetPath)
		if statErr != nil {
			if os.IsNotExist(statErr) && f.MissingBefore {
				record.InstalledStateIDs[i] = repairPlanReleaseNodeState(f.TargetPath)
				continue
			}
			return nil, fmt.Errorf("record installed update: inspect %s: %w", filepath.Base(f.TargetPath), statErr)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("record installed update: %s is not a regular file", filepath.Base(f.TargetPath))
		}
		return nil, fmt.Errorf("record installed update: publish receipt is missing for %s", filepath.Base(f.TargetPath))
	}
	if len(receiptStates) != 0 {
		return nil, fmt.Errorf("record installed update: publish receipt target is outside the release unit")
	}
	for i, f := range current.Files {
		if err := verifyRepairPlanReleaseNodeStateFor(f.TargetPath, f.TargetPath, record.InstalledStateIDs[i]); err != nil {
			return nil, fmt.Errorf("record installed update: release unit changed while recording: %w", err)
		}
	}
	if err := createInstalledFileUpdateState(current, record); err != nil {
		return nil, fmt.Errorf("record installed update: %w", err)
	}
	installedUpdateAfterCreate(installedFileUpdateStatePath(current))
	latest, err := readPendingUpdateForLauncher(claimed.TargetPath)
	if err != nil {
		return nil, fmt.Errorf("record installed update: re-read pending transaction: %w", err)
	}
	if !reflect.DeepEqual(current, latest) {
		return nil, fmt.Errorf("record installed update: pending transaction changed")
	}
	if _, _, err := installedFileUpdateTargets(latest, true); err != nil {
		return nil, fmt.Errorf("record installed update: %w", err)
	}
	return latest, nil
}

func installedFileUpdateStatePath(tx *UpdateTransaction) string {
	if tx == nil {
		return ""
	}
	transactionID := UpdateTransactionID(tx)
	root := config.MemoryUserDir()
	if root == "" || len(transactionID) != sha256.Size*2 {
		return ""
	}
	return filepath.Join(root, "repair", "updates", transactionID+".installed.json")
}

func createInstalledFileUpdateState(tx *UpdateTransaction, record *installedFileUpdateState) error {
	if err := validateInstalledFileUpdateState(tx, record); err != nil {
		return err
	}
	path := installedFileUpdateStatePath(tx)
	if path == "" {
		return fmt.Errorf("installed release-unit state path is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if !repairNodeInsideResolvedRoot(filepath.Join(config.MemoryUserDir(), "repair"), path) {
		return fmt.Errorf("installed release-unit state resolves outside the repair directory")
	}
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := fileutil.AtomicCreateFile(path, append(b, '\n'), 0o600); err == nil {
		return nil
	} else if !os.IsExist(err) {
		return err
	}
	existing, err := readInstalledFileUpdateState(tx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(existing, record) {
		return fmt.Errorf("installed release-unit state already exists with different content")
	}
	return nil
}

func readInstalledFileUpdateState(tx *UpdateTransaction) (*installedFileUpdateState, error) {
	path := installedFileUpdateStatePath(tx)
	if path == "" {
		return nil, fmt.Errorf("installed release-unit state path is unavailable")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("installed release-unit state is not a regular file")
	}
	if !repairNodeInsideResolvedRoot(filepath.Join(config.MemoryUserDir(), "repair"), path) {
		return nil, fmt.Errorf("installed release-unit state resolves outside the repair directory")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record installedFileUpdateState
	if err := json.Unmarshal(b, &record); err != nil {
		return nil, err
	}
	if err := validateInstalledFileUpdateState(tx, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

func validateInstalledFileUpdateState(tx *UpdateTransaction, record *installedFileUpdateState) error {
	if tx == nil || tx.TargetKind != "file" || len(tx.Files) == 0 ||
		record == nil || record.SchemaVersion != 1 ||
		record.UpdateTransactionID != UpdateTransactionID(tx) ||
		len(record.InstalledStateIDs) != len(tx.Files) {
		return fmt.Errorf("installed release-unit state is incomplete")
	}
	for _, stateID := range record.InstalledStateIDs {
		stateID = strings.TrimSpace(stateID)
		if len(stateID) != sha256.Size*2 {
			return fmt.Errorf("installed release-unit state is invalid")
		}
		if _, err := hex.DecodeString(stateID); err != nil {
			return fmt.Errorf("installed release-unit state is invalid")
		}
	}
	return nil
}

func installedFileUpdateTargets(
	tx *UpdateTransaction,
	requireBinding bool,
) ([]UpdateTransactionFile, bool, error) {
	if tx == nil || tx.TargetKind != "file" || len(tx.Files) == 0 {
		if requireBinding {
			return nil, false, fmt.Errorf("installed release-unit state is missing")
		}
		return pendingUpdateFiles(tx), false, nil
	}
	files := append([]UpdateTransactionFile(nil), tx.Files...)
	bound := 0
	for _, f := range files {
		if strings.TrimSpace(f.InstalledStateID) != "" {
			bound++
		}
	}
	if bound != 0 && bound != len(files) {
		return nil, false, fmt.Errorf("installed release-unit state is incomplete")
	}
	if bound == 0 {
		record, err := readInstalledFileUpdateState(tx)
		if err != nil {
			if os.IsNotExist(err) {
				if requireBinding {
					return nil, false, fmt.Errorf("installed release-unit state is missing")
				}
				return files, false, nil
			}
			return nil, false, err
		}
		for i := range files {
			files[i].InstalledStateID = record.InstalledStateIDs[i]
		}
	}
	for _, f := range files {
		if err := verifyRepairPlanReleaseNodeStateFor(f.TargetPath, f.TargetPath, f.InstalledStateID); err != nil {
			return nil, true, fmt.Errorf("installed release file %s changed: %w", filepath.Base(f.TargetPath), err)
		}
	}
	return files, true, nil
}

func removeInstalledFileUpdateState(tx *UpdateTransaction) error {
	record, err := readInstalledFileUpdateState(tx)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	path := installedFileUpdateStatePath(tx)
	expectedState := repairPlanFileState(path)
	return removeUpdateNodeMatching(path, func(moved string) error {
		if err := verifyRepairPlanStateIDFor(moved, path, expectedState); err != nil {
			return err
		}
		b, err := os.ReadFile(moved)
		if err != nil {
			return err
		}
		var actual installedFileUpdateState
		if err := json.Unmarshal(b, &actual); err != nil {
			return err
		}
		if !reflect.DeepEqual(&actual, record) {
			return fmt.Errorf("installed release-unit state changed before cleanup")
		}
		return nil
	}, false)
}
