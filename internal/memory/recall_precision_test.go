// Recall precision tests: verify that automatic recall selects relevant facts,
// excludes irrelevant ones, and correctly handles conflict resolution.
package memory

import (
	"testing"
	"time"
)

// seedTestFact creates a memory in the store with the given fields and returns
// it for inspection. Scope defaults to project if empty.
func seedTestFact(t *testing.T, store Store, name, title, body, subjectKey string, opts ...func(*Memory)) Memory {
	t.Helper()
	m := Memory{
		Name:        name,
		Title:       title,
		Description: title,
		Type:        TypeProject,
		Scope:       FactScopeProject,
		Body:        body,
		SubjectKey:  subjectKey,
	}
	for _, o := range opts {
		o(&m)
	}
	if _, err := store.Save(m); err != nil {
		t.Fatal(err)
	}
	saved, ok := store.Read(m.Name)
	if !ok {
		t.Fatalf("failed to read back saved memory %q", m.Name)
	}
	return saved
}

func TestRecallPrecisionRelevantFact(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	seedTestFact(t, store, "project-uses-go", "Project uses Go",
		"The project is written in Go. Use gofmt for formatting.", "project.language")

	// Query with two matching terms ("project" and "go") to trigger strongRecallMatch.
	result := AutoRecall(store, "project Go code style", RecallOptions{Now: time.Now()})
	if len(result.Hits) == 0 {
		t.Fatal("expected at least one recall hit for a relevant query")
	}
	if result.Hits[0].Memory.Name != "project-uses-go" {
		t.Errorf("expected Go fact to be recalled first, got %q", result.Hits[0].Memory.Name)
	}
}

func TestRecallPrecisionIrrelevantFact(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	seedTestFact(t, store, "user-likes-rust", "User likes Rust",
		"The user prefers Rust for new projects.", "user.language_preference")

	// Query about package.json has nothing to do with Rust preferences.
	result := AutoRecall(store, "modify package.json dependencies", RecallOptions{Now: time.Now()})
	if len(result.Hits) > 0 {
		for _, h := range result.Hits {
			if h.Memory.Name == "user-likes-rust" {
				t.Error("Rust preference should NOT be recalled for a package.json query")
			}
		}
	}
}

func TestRecallPrecisionConflictResolution(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Seed a DuckDB memory.
	duckdb := seedTestFact(t, store, "database-duckdb", "Database is DuckDB",
		"The project uses DuckDB as its primary database engine.", "project.database")

	// Update to PostgreSQL via the dedup mechanism.
	duckdb.Body = "The project uses PostgreSQL as its primary database engine."
	duckdb.Description = "Database is PostgreSQL"
	duckdb.Title = "Database is PostgreSQL"
	duckdb.Revision++
	if _, err := store.Save(duckdb); err != nil {
		t.Fatal(err)
	}

	// Query about database engine should recall PostgreSQL, not DuckDB.
	result := AutoRecall(store, "project database engine", RecallOptions{Now: time.Now()})
	if len(result.Hits) == 0 {
		t.Fatal("expected at least one recall hit")
	}
	for _, h := range result.Hits {
		if h.Memory.Body == "The project uses DuckDB as its primary database engine." {
			t.Error("old DuckDB fact should not be recalled after migration to PostgreSQL")
		}
	}
	if len(result.Hits) > 0 && result.Hits[0].Memory.Body != "The project uses PostgreSQL as its primary database engine." {
		t.Errorf("expected PostgreSQL fact to be recalled, got %q", result.Hits[0].Memory.Body)
	}
}

func TestRecallPrecisionExpiredMemory(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	pastTime := time.Now().Add(-24 * time.Hour)
	seedTestFact(t, store, "expired-fact", "Expired fact",
		"This fact has expired and should not be recalled.", "project.temp",
		func(m *Memory) {
			m.ExpiresAt = pastTime
		})

	result := AutoRecall(store, "expired fact", RecallOptions{Now: time.Now()})
	for _, h := range result.Hits {
		if h.Memory.Name == "expired-fact" {
			t.Error("expired fact should NOT be recalled")
		}
	}
}

func TestRecallPrecisionPinnedMemoryExcluded(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	seedTestFact(t, store, "pinned-rule", "Always use tabs",
		"Always use tabs for indentation.", "project.style",
		func(m *Memory) {
			m.Activation = ActivationPinned
		})

	result := AutoRecall(store, "indentation style tabs", RecallOptions{Now: time.Now()})
	for _, h := range result.Hits {
		if h.Memory.Name == "pinned-rule" {
			t.Error("pinned memory should NOT appear in recall results (it rides the stable prefix)")
		}
	}
}

func TestRecallPrecisionManualMemoryRankedHigher(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Two facts with different origins. Use different subject keys to avoid
	// subject-key conflict.
	seedTestFact(t, store, "manual-fact", "Manual editor preference",
		"The user prefers vim as their primary editor.", "user.editor_primary",
		func(m *Memory) { m.Origin = OriginManual })
	seedTestFact(t, store, "consolidated-fact", "Consolidated editor observation",
		"The user might prefer vim based on session history.", "user.editor_observed",
		func(m *Memory) {
			m.Origin = OriginConsolidated
			m.Confidence = 0.8
		})

	result := AutoRecall(store, "user editor vim preference", RecallOptions{Now: time.Now()})
	if len(result.Hits) < 2 {
		t.Fatalf("expected at least 2 hits, got %d", len(result.Hits))
	}
	// The manual fact should rank higher (higher score) than the consolidated one.
	manualIdx := -1
	consolidatedIdx := -1
	for i, h := range result.Hits {
		if h.Memory.Name == "manual-fact" {
			manualIdx = i
		}
		if h.Memory.Name == "consolidated-fact" {
			consolidatedIdx = i
		}
	}
	if manualIdx < 0 {
		t.Fatal("manual-origin fact should appear in recall results")
	}
	if consolidatedIdx < 0 {
		t.Fatal("consolidated-origin fact should appear in recall results")
	}
	if manualIdx > consolidatedIdx {
		t.Errorf("manual-origin fact (idx=%d) should rank above consolidated-origin fact (idx=%d)", manualIdx, consolidatedIdx)
	}
}

func TestRecallPrecisionStaleMemoryPenalized(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// A stale fact (updated 200 days ago with stable volatility).
	staleTime := time.Now().Add(-200 * 24 * time.Hour)
	seedTestFact(t, store, "stale-fact", "Stale editor preference",
		"The user uses vim as their editor.", "user.editor_stale",
		func(m *Memory) {
			m.UpdatedAt = staleTime
			m.Volatility = VolatilityStable
		})

	// A fresh fact with the same subject.
	seedTestFact(t, store, "fresh-fact", "Fresh editor preference",
		"The user uses neovim as their editor.", "user.editor_fresh",
		func(m *Memory) { m.Volatility = VolatilityStable })

	result := AutoRecall(store, "user editor vim neovim", RecallOptions{Now: time.Now()})
	if len(result.Hits) < 2 {
		t.Fatalf("expected at least 2 hits, got %d", len(result.Hits))
	}
	// Fresh fact should rank higher than stale one.
	freshIdx := -1
	staleIdx := -1
	for i, h := range result.Hits {
		if h.Memory.Name == "fresh-fact" {
			freshIdx = i
		}
		if h.Memory.Name == "stale-fact" {
			staleIdx = i
		}
	}
	if freshIdx < 0 || staleIdx < 0 {
		t.Fatalf("both facts should appear in results: fresh=%d stale=%d", freshIdx, staleIdx)
	}
	if freshIdx > staleIdx {
		t.Errorf("fresh fact (idx=%d) should rank above stale fact (idx=%d)", freshIdx, staleIdx)
	}
}

func TestRecallPrecisionGenericQuerySuppressed(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	seedTestFact(t, store, "some-fact", "Some fact", "Some important fact.", "project.info")

	// Generic queries like "continue" or "ok" should be suppressed.
	for _, q := range []string{"continue", "ok", "yes", "next"} {
		result := AutoRecall(store, q, RecallOptions{Now: time.Now()})
		if result.Suppressed == "" {
			t.Errorf("query %q should be suppressed as generic", q)
		}
	}
}

func TestRecallPrecisionSubjectKeyDedup(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Seed a global fact with a subject key.
	seedTestFact(t, store, "global-db", "Global database",
		"Use PostgreSQL globally for all projects.", "project.database",
		func(m *Memory) { m.Scope = FactScopeGlobal })

	// Update it to a project-scoped fact (simulating a project override).
	existing, ok := store.Read("global-db")
	if !ok {
		t.Fatal("global-db not found")
	}
	existing.Scope = FactScopeProject
	existing.Body = "Use SQLite in this specific project."
	existing.Description = "Project database is SQLite"
	existing.Title = "Project database is SQLite"
	existing.Revision++
	if _, err := store.Save(existing); err != nil {
		t.Fatal(err)
	}

	// Recall should return the updated fact.
	result := AutoRecall(store, "project database", RecallOptions{Now: time.Now()})
	if len(result.Hits) == 0 {
		t.Fatal("expected at least one recall hit")
	}
	// The fact should be the updated project-scoped one.
	found := false
	for _, h := range result.Hits {
		if h.Memory.Body == "Use SQLite in this specific project." {
			found = true
			break
		}
	}
	if !found {
		t.Error("updated project-scoped fact should be recalled")
	}
}
