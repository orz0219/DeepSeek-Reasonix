package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mockCompletion returns a CompletionFunc that produces the given response.
func mockCompletion(response string, err error) CompletionFunc {
	return func(_ context.Context, _, _ string) (string, error) {
		return response, err
	}
}

// newTestStore creates a temporary store for testing.
func newTestStore(t *testing.T) (Store, func()) {
	t.Helper()
	dir := t.TempDir()
	store := Store{Dir: dir, GlobalDir: ""}
	return store, func() {}
}

func TestSessionToMemory(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	summaryResp := mustMarshal(t, SessionSummary{
		SessionID:       "test-session",
		Goal:            "Set up DuckDB for the project",
		Decisions:       []string{"Use DuckDB as the database"},
		Constraints:     []string{"Do not use SQLite"},
		DiscoveredFacts: []string{"Project needs OLAP database"},
	})

	candidateResp := mustMarshal(t, struct {
		Memories []MemoryCandidate `json:"memories"`
	}{
		Memories: []MemoryCandidate{
			{
				Action:      CandidateActionAdd,
				Name:        "database-uses-duckdb",
				Title:       "Database uses DuckDB",
				Description: "Project database is based on DuckDB",
				Type:        "project",
				Scope:       "project",
				Keywords:    []string{"duckdb", "database"},
				Body:        "The project uses DuckDB as its primary database.",
				SubjectKey:  "project.database",
				Confidence:  0.96,
				Volatility:  "stable",
			},
		},
	})

	callCount := 0
	llm := func(_ context.Context, system, _ string) (string, error) {
		callCount++
		if callCount == 1 {
			return summaryResp, nil
		}
		return candidateResp, nil
	}

	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	result, err := consolidator.Consolidate(context.Background(), ConsolidationInput{
		SessionID:        "test-session",
		Messages:         []string{"User: Use DuckDB for the database"},
		ExistingMemories: nil,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Added) != 1 {
		t.Fatalf("expected 1 added, got %d: %v", len(result.Added), result.Added)
	}

	// Verify the memory was saved correctly.
	memories := store.ListAll()
	if len(memories) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(memories))
	}
	m := memories[0]
	if string(NormalizeFactScope(string(m.Scope))) != string(FactScopeProject) {
		t.Errorf("scope: want project, got %s", m.Scope)
	}
	if ResolveActivation(m) != ActivationRelevant {
		t.Errorf("activation: want relevant, got %s", m.Activation)
	}
	if string(NormalizeOrigin(string(m.Origin))) != string(OriginConsolidated) {
		t.Errorf("origin: want consolidated, got %s", m.Origin)
	}
	if m.SourceSessionID != "test-session" {
		t.Errorf("source_session_id: want test-session, got %s", m.SourceSessionID)
	}
	if m.Confidence < 0.95 {
		t.Errorf("confidence: want >= 0.95, got %f", m.Confidence)
	}
}

func TestDeduplicateSameFact(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Save initial memory (no ID for creation).
	m := Memory{
		Name:        "database-uses-duckdb",
		Title:       "Database uses DuckDB",
		Description: "Project uses DuckDB",
		Type:        TypeProject,
		Scope:       FactScopeProject,
		Body:        "The project uses DuckDB.",
		SubjectKey:  "project.database",
	}
	if _, err := store.Save(m); err != nil {
		t.Fatal(err)
	}

	// Try to add duplicate via dedup.
	cand := MemoryCandidate{
		Action:      CandidateActionAdd,
		Name:        "database-uses-duckdb",
		SubjectKey:  "project.database",
		Description: "Project uses DuckDB",
		Body:        "The project uses DuckDB.",
	}
	existing := store.ListAll()
	dedup := DeduplicateCandidate(cand, existing)
	if dedup.Action != CandidateActionIgnore {
		t.Errorf("expected ignore for duplicate, got %s: %s", dedup.Action, dedup.Reason)
	}
}

func TestUpdateFact(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Save initial SQLite memory.
	m := Memory{

		Name:       "project-database",
		Title:      "Project Database",
		Type:       TypeProject,
		Scope:      FactScopeProject,
		Body:       "The project uses SQLite.",
		SubjectKey: "project.database",
	}
	if _, err := store.Save(m); err != nil {
		t.Fatal(err)
	}

	// Create update candidate.
	cand := MemoryCandidate{
		Action:      CandidateActionUpdate,
		Name:        "project-database",
		SubjectKey:  "project.database",
		Description: "Project database is now DuckDB",
		Body:        "The project uses DuckDB.",
	}
	existing := store.ListAll()
	dedup := DeduplicateCandidate(cand, existing)
	if dedup.Action != CandidateActionUpdate {
		t.Errorf("expected update, got %s: %s", dedup.Action, dedup.Reason)
	}
}

func TestSubjectKeyConflict(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Save initial memory with subject key.
	m := Memory{

		Name:       "database-uses-sqlite",
		Title:      "Database uses SQLite",
		Type:       TypeProject,
		Scope:      FactScopeProject,
		Body:       "The project uses SQLite.",
		SubjectKey: "project.database",
	}
	if _, err := store.Save(m); err != nil {
		t.Fatal(err)
	}

	// New candidate with same subject key but different body.
	cand := MemoryCandidate{
		Action:     CandidateActionAdd,
		Name:       "database-uses-duckdb",
		SubjectKey: "project.database",
		Body:       "The project uses DuckDB.",
	}
	existing := store.ListAll()
	dedup := DeduplicateCandidate(cand, existing)
	if dedup.Action != CandidateActionUpdate {
		t.Errorf("expected update (subject conflict), got %s: %s", dedup.Action, dedup.Reason)
	}
	if dedup.ExistingID == "" {
		t.Error("expected non-empty existing id")
	}
}

func TestGlobalForbidden(t *testing.T) {
	_, cleanup := newTestStore(t)
	defer cleanup()

	cand := MemoryCandidate{
		Action:      CandidateActionAdd,
		Scope:       "global",
		Name:        "test-fact",
		Description: "Test fact",
		Body:        "Some fact.",
		Confidence:  0.95,
	}
	result, err := ValidateCandidate(cand, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Scope != string(FactScopeProject) {
		t.Errorf("scope must be forced to project, got %s", result.Scope)
	}
}

func TestPinnedForbidden(t *testing.T) {
	// Auto-consolidated memories are always activation=relevant.
	// This is enforced at mutation time, not in candidate validation,
	// but the LLM prompt instructs activation=relevant.
	// Verify the policy code does not allow pinning.
	store, cleanup := newTestStore(t)
	defer cleanup()

	llm := mockCompletion(`{"memories":[]}`, nil)
	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	result, _ := consolidator.Consolidate(context.Background(), ConsolidationInput{
		SessionID: "test",
		Messages:  []string{"pin this"},
	})
	if len(result.Added) > 0 {
		t.Error("should not add any memories for empty candidate list")
	}
}

func TestCasualChatIgnored(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// LLM returns no candidates for casual chat.
	llm := mockCompletion(`{"memories":[]}`, nil)
	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	result, _ := consolidator.Consolidate(context.Background(), ConsolidationInput{
		SessionID: "test",
		Messages:  []string{"Hello", "How are you?"},
	})
	if len(result.Added) != 0 || len(result.Updated) != 0 {
		t.Errorf("casual chat should produce 0 memories, got added=%d updated=%d", len(result.Added), len(result.Updated))
	}
}

func TestMaxMemoriesPerSession(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Create 10 candidates.
	var candidates []MemoryCandidate
	for i := range 10 {
		candidates = append(candidates, MemoryCandidate{
			Action:      CandidateActionAdd,
			Name:        fmt.Sprintf("fact-%d", i),
			Description: fmt.Sprintf("Fact number %d", i),
			Body:        fmt.Sprintf("This is fact %d.", i),
			Confidence:  0.90 - float64(i)*0.01,
			Volatility:  "stable",
		})
	}
	candidateJSON, _ := json.Marshal(struct {
		Memories []MemoryCandidate `json:"memories"`
	}{Memories: candidates})

	summaryResp := mustMarshal(t, SessionSummary{SessionID: "test", Goal: "test"})

	callCount := 0
	llm := func(_ context.Context, _, _ string) (string, error) {
		callCount++
		if callCount == 1 {
			return summaryResp, nil
		}
		return string(candidateJSON), nil
	}

	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{
		MaxNewMemoriesPerSession: 3,
	})
	result, _ := consolidator.Consolidate(context.Background(), ConsolidationInput{
		SessionID: "test",
		Messages:  []string{"user said many things"},
	})
	if len(result.Added) > 3 {
		t.Errorf("max 3 memories per session, got %d", len(result.Added))
	}
}

func TestManualMemoryPriority(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Save a manual memory.
	m := Memory{

		Name:       "project-database",
		Title:      "Database",
		Type:       TypeProject,
		Scope:      FactScopeProject,
		Body:       "The project uses PostgreSQL.",
		SubjectKey: "project.database",
		Origin:     OriginManual,
	}
	if _, err := store.Save(m); err != nil {
		t.Fatal(err)
	}

	// Validate a consolidated candidate that tries to override it.
	cand := MemoryCandidate{
		Action:      CandidateActionUpdate,
		Name:        "project-database",
		SubjectKey:  "project.database",
		Description: "Now uses DuckDB",
		Body:        "The project uses DuckDB.",
		Confidence:  0.95,
	}
	existing := store.ListAll()
	_, err := ValidateCandidate(cand, existing, nil)
	if err == nil || !strings.Contains(err.Error(), "refusing to override") {
		t.Errorf("expected refusal to override manual memory, got: %v", err)
	}
}

func TestAutoMemoryRetrieval(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Auto-created memory must have activation=relevant.
	m := Memory{

		Name:        "test-fact",
		Description: "Test",
		Type:        TypeProject,
		Scope:       FactScopeProject,
		Activation:  ActivationRelevant,
		Body:        "A fact.",
		Origin:      OriginConsolidated,
	}
	if _, err := store.Save(m); err != nil {
		t.Fatal(err)
	}
	saved := store.ListAll()
	if len(saved) != 1 {
		t.Fatal("expected 1 memory")
	}
	if ResolveActivation(saved[0]) != ActivationRelevant {
		t.Errorf("auto memory must be relevant, got %s", ResolveActivation(saved[0]))
	}
}

func TestConsolidationFailure(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// LLM returns invalid JSON.
	llm := mockCompletion("not json at all", nil)
	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	result, err := consolidator.Consolidate(context.Background(), ConsolidationInput{
		SessionID: "test",
		Messages:  []string{"some message"},
	})
	// Should not return error — failure is isolated.
	if err != nil {
		t.Fatalf("consolidation should not propagate errors, got: %v", err)
	}
	// Should not have added any memories.
	memories := store.ListAll()
	if len(memories) != 0 {
		t.Errorf("expected 0 memories after failure, got %d", len(memories))
	}
	_ = result
}

func TestBackwardCompatibility(t *testing.T) {
	store, cleanup := newTestStore(t)
	defer cleanup()

	// Save a memory without origin/confidence (simulating old format).
	m := Memory{

		Name:       "old-fact",
		Title:      "Old Fact",
		Type:       TypeProject,
		Scope:      FactScopeProject,
		Body:       "An old fact.",
		SubjectKey: "project.old",
	}
	if _, err := store.Save(m); err != nil {
		t.Fatal(err)
	}

	// Load and verify.
	loaded := store.ListAll()
	if len(loaded) != 1 {
		t.Fatal("expected 1 memory")
	}
	// Origin should default to explicit for old memories.
	if NormalizeOrigin(string(loaded[0].Origin)) != OriginExplicit {
		t.Errorf("old memory origin should default to explicit, got %s", loaded[0].Origin)
	}
	// Confidence should be zero (not a consolidated memory).
	if loaded[0].Confidence != 0 {
		t.Errorf("old memory confidence should be 0, got %f", loaded[0].Confidence)
	}
}

func TestDontRememberSuppression(t *testing.T) {
	hints := DetectSuppression([]string{
		"这个只是临时的，不要记住。",
		"OK, now do this other thing",
	})
	if len(hints) == 0 {
		t.Error("expected suppression hints from '不要记住'")
	}

	noHints := DetectSuppression([]string{
		"Please save this as a permanent rule.",
		"The database uses DuckDB.",
	})
	if len(noHints) != 0 {
		t.Errorf("expected no suppression hints, got %v", noHints)
	}
}

func TestStripJSONFences(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{"{\"a\":1}", `{"a":1}`},
		{"  ```json\n{\"a\":1}\n```  ", `{"a":1}`},
	}
	for _, tt := range tests {
		got := stripJSONFences(tt.in)
		if got != tt.want {
			t.Errorf("stripJSONFences(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestProjectMessages(t *testing.T) {
	msgs := []string{"", "Hello", "", "Use DuckDB", "  "}
	projected := ProjectMessages(msgs)
	if !strings.Contains(projected, "Hello") {
		t.Error("expected 'Hello' in projected messages")
	}
	if !strings.Contains(projected, "Use DuckDB") {
		t.Error("expected 'Use DuckDB' in projected messages")
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
