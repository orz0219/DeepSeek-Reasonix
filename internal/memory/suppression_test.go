package memory

import (
	"testing"
)

func TestSuppressionPersistence(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Initially empty.
	supps := store.LoadSuppressions()
	if len(supps) != 0 {
		t.Fatalf("expected 0 suppressions, got %d", len(supps))
	}

	// Save one.
	if err := store.SaveSuppression(Suppression{
		SubjectKey: "project.database.schema",
		SessionID:  "session-1",
		Reason:     "user said don't remember",
	}); err != nil {
		t.Fatal(err)
	}

	// Load and verify.
	supps = store.LoadSuppressions()
	if len(supps) != 1 {
		t.Fatalf("expected 1 suppression, got %d", len(supps))
	}
	if supps[0].SubjectKey != "project.database.schema" {
		t.Errorf("subject_key: want project.database.schema, got %s", supps[0].SubjectKey)
	}

	// Duplicate save should be a no-op.
	if err := store.SaveSuppression(Suppression{
		SubjectKey: "project.database.schema",
		SessionID:  "session-2",
	}); err != nil {
		t.Fatal(err)
	}
	supps = store.LoadSuppressions()
	if len(supps) != 1 {
		t.Errorf("expected 1 suppression after duplicate, got %d", len(supps))
	}
}

func TestIsSuppressed(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	if store.IsSuppressed("project.database") {
		t.Error("should not be suppressed initially")
	}

	_ = store.SaveSuppression(Suppression{SubjectKey: "project.database"})

	if !store.IsSuppressed("project.database") {
		t.Error("should be suppressed after save")
	}
	if !store.IsSuppressed("project.Database") {
		t.Error("suppression check should be case-insensitive")
	}
	if store.IsSuppressed("project.api") {
		t.Error("different subject should not be suppressed")
	}
}

func TestDetectSuppression(t *testing.T) {
	tests := []struct {
		name     string
		messages []string
		wantLen  int
	}{
		{"chinese dont remember", []string{"这个不要记住"}, 1},
		{"chinese temporary", []string{"这只是临时的"}, 1},
		{"english dont save", []string{"don't save this"}, 1},
		{"english just for now", []string{"just for now"}, 1},
		{"no suppression", []string{"Please remember this forever"}, 0},
		{"empty", []string{}, 0},
		{"mixed", []string{"Hello", "不要记这个", "OK"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints := DetectSuppression(tt.messages)
			if len(hints) != tt.wantLen {
				t.Errorf("got %d hints, want %d: %v", len(hints), tt.wantLen, hints)
			}
		})
	}
}

func TestSuppressionNormalization(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Save with spaces and mixed case.
	_ = store.SaveSuppression(Suppression{SubjectKey: "Project.Database.Schema"})

	if !store.IsSuppressed("project.database.schema") {
		t.Error("normalized key should match")
	}
	if !store.IsSuppressed("PROJECT.DATABASE.SCHEMA") {
		t.Error("uppercase should match")
	}
}
