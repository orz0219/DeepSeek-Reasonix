package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Store) failTransaction(tx *TransactionManifest, targets []TransactionTarget, stages []FileStage, cause error) error {
	return s.failTransactionAfterStateCompensation(tx, targets, stages, cause, nil)
}

// failTransactionAfterStateCompensation compensates files and records whether
// the conversation/checkpoint side was also restored. Any incomplete side keeps
// the manifest committing so startup can retry the whole compensation.
func (s *Store) failTransactionAfterStateCompensation(tx *TransactionManifest, targets []TransactionTarget, stages []FileStage, cause, stateCompensationErr error) error {
	if tx != nil {
		targets = tx.Targets
	}
	compensationErr := s.compensatePublished(targets, stages)
	combined := errors.Join(cause, stateCompensationErr)
	if compensationErr != nil {
		combined = errors.Join(combined, fmt.Errorf("compensation failed: %w", compensationErr))
	}
	if compensationErr != nil || stateCompensationErr != nil {

		tx.State = TxCommitting
		tx.Error = combined.Error()
		tx.UpdatedAt = time.Now()
		if persistErr := s.persistTransaction(tx); persistErr != nil {
			combined = errors.Join(combined, fmt.Errorf("persist pending compensation: %w", persistErr))
		}
		return combined
	}
	if abortErr := s.abortTransaction(tx, combined); abortErr != nil {
		combined = errors.Join(combined, fmt.Errorf("persist aborted transaction: %w", abortErr))
	}
	return combined
}

func (s *Store) abortTransaction(tx *TransactionManifest, cause error) error {
	tx.State = TxAborted
	tx.Error = cause.Error()
	tx.UpdatedAt = time.Now()
	return s.persistTransaction(tx)
}

func (s *Store) persistTransaction(tx *TransactionManifest) error {
	if s.dir == "" {
		return nil
	}
	return writeJSONAtomic(s.txManifestPath(tx.ID), tx)
}

func (s *Store) txDir() string {
	if s.dir == "" {
		return filepath.Join(os.TempDir(), "reasonix-ckpt-tx")
	}
	return filepath.Join(s.dir, "transactions")
}

func (s *Store) txManifestPath(id string) string {
	return filepath.Join(s.txDir(), id+".json")
}

// RecoverTransactions scans for incomplete file-only transactions. Conversation
// transactions are intentionally deferred until the controller has installed the
// resumed session and can provide a ConversationApplier.
func (s *Store) RecoverTransactions() []string {
	return s.recoverTransactions(nil)
}

// RecoverTransactionsWithApplier finishes startup recovery after the resumed
// conversation is live. A committing rewind first restores its forward
// transcript/checkpoints, then compensates files; a committing undo first
// reapplies its parent rewind, then compensates files. The manifest remains
// committing if either side fails so a later startup can retry idempotently.
func (s *Store) RecoverTransactionsWithApplier(applier ConversationApplier) []string {
	return s.recoverTransactions(applier)
}

func (s *Store) recoverTransactions(applier ConversationApplier) []string {
	if s == nil || s.dir == "" {
		return nil
	}
	dir := s.txDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	undoneParents := map[string]bool{}
	for _, entry := range ents {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var tx TransactionManifest
		if readJSONFile(filepath.Join(dir, entry.Name()), &tx) == nil && tx.State == TxCommitted && tx.Kind == "undo" && tx.ParentTransaction != "" {
			undoneParents[tx.ParentTransaction] = true
		}
	}
	var notes []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var tx TransactionManifest
		if err := readJSONFile(filepath.Join(dir, e.Name()), &tx); err != nil {
			continue
		}
		switch tx.State {
		case TxPrepared:

			for _, target := range tx.Targets {
				if target.PublishTmp != "" {
					_ = secureRemove(s.root, target.PublishTmp)
				}
			}
			tx.State = TxAborted
			tx.Error = "abandoned prepared transaction on recovery"
			tx.UpdatedAt = time.Now()
			_ = s.persistTransaction(&tx)
			notes = append(notes, fmt.Sprintf("aborted prepared %s", tx.ID))
		case TxCommitting:
			needsConversation := tx.Scope == RewindConversation || tx.Scope == RewindBoth
			if needsConversation && applier == nil {
				notes = append(notes, fmt.Sprintf("deferred conversation recovery %s", tx.ID))
				continue
			}
			if needsConversation {
				var restoreErr error
				if tx.Kind != "undo" && tx.HasBoundary && len(tx.ConversationForward) == 0 {
					restoreErr = fmt.Errorf("missing forward conversation payload")
				} else if tx.Kind == "undo" {
					if tx.HasBoundary {
						restoreErr = errors.Join(restoreErr, applier.ApplyConversationTruncate(tx.BoundaryIndex, tx.ConversationForward))
					}
					restoreErr = errors.Join(restoreErr, applier.TruncateCheckpoints(tx.TruncateFrom))
				} else {
					restoreErr = s.restoreTransactionConversation(&tx, applier)
				}
				if restoreErr != nil {
					notes = append(notes, fmt.Sprintf("conversation recovery %s pending: %v", tx.ID, restoreErr))
					tx.Error = fmt.Sprintf("crash recovery conversation compensation pending: %v", restoreErr)
					tx.UpdatedAt = time.Now()
					_ = s.persistTransaction(&tx)
					continue
				}
			}

			stages := make([]FileStage, len(tx.Targets))
			for i, t := range tx.Targets {
				stages[i] = FileStage{Path: t.Path, Phase: "compensate"}
			}
			if err := s.compensatePublished(tx.Targets, stages); err != nil {
				notes = append(notes, fmt.Sprintf("compensate %s: %v", tx.ID, err))
				tx.Error = fmt.Sprintf("crash recovery compensation pending: %v", err)
				tx.UpdatedAt = time.Now()
				_ = s.persistTransaction(&tx)
			} else {
				notes = append(notes, fmt.Sprintf("compensated committing %s", tx.ID))
				tx.State = TxAborted
				tx.Error = "compensated after crash during commit"
				tx.UpdatedAt = time.Now()
				_ = s.persistTransaction(&tx)
			}
		case TxCommitted:
			if tx.Kind == "undo" || undoneParents[tx.ID] {
				continue
			}

			s.mu.Lock()
			if s.lastUndo == nil || s.lastUndo.UpdatedAt.Before(tx.UpdatedAt) {
				cp := tx
				s.lastUndo = &cp
			}
			s.mu.Unlock()
		}
	}
	return notes
}
