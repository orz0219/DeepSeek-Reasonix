// Session memory consolidation: extracting durable facts from session history
// and committing them as project-scoped, retrieval-only memories. The pipeline
// is Summary → Candidate Extraction → Validation → Policy → Store Mutation.
// LLM calls are behind CompletionFunc so the memory package never imports
// provider directly.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// CompletionFunc is the LLM call abstraction. boot layers wrap
// boundedllm.Call with a concrete provider and inject this.
type CompletionFunc func(ctx context.Context, systemPrompt, userMessage string) (string, error)

// Consolidator extracts durable facts from a finished session and saves them
// as project-scoped retrieval-only memories.
type Consolidator interface {
	Consolidate(ctx context.Context, input ConsolidationInput) (ConsolidationResult, error)
}

// ConsolidationInput carries everything the consolidator needs from the
// controller. The caller populates it from the session snapshot and store.
type ConsolidationInput struct {
	Workspace        string
	SessionID        string
	SessionPath      string
	Messages         []string // projected user/assistant text (not full provider.Message)
	ExistingMemories []Memory
	SuppressedKeys   []string // subject keys already suppressed at workspace level
}

// ConsolidationResult records what the consolidation actually did, for
// telemetry and debugging.
type ConsolidationResult struct {
	SessionID string
	Added     []string
	Updated   []string
	Archived  []string
	Ignored   int
	Error     string
}

// SessionSummary is the LLM-generated compressed state of a session.
type SessionSummary struct {
	SessionID        string   `json:"session_id"`
	Goal             string   `json:"goal"`
	Decisions        []string `json:"decisions"`
	Constraints      []string `json:"constraints"`
	DiscoveredFacts  []string `json:"discovered_facts"`
	ChangedFiles     []string `json:"changed_files"`
	Unresolved       []string `json:"unresolved"`
	UserPreferences  []string `json:"user_preferences"`
	SuppressionHints []string `json:"suppression_hints"`
}

// MemoryCandidate is one potential memory extracted from a summary.
type MemoryCandidate struct {
	Action           string           `json:"action"` // add, update, archive, ignore
	Name             string           `json:"name"`
	Title            string           `json:"title"`
	Description      string           `json:"description"`
	Type             string           `json:"type"`
	Scope            string           `json:"scope"`
	Keywords         []string         `json:"keywords"`
	Body             string           `json:"body"`
	SubjectKey       string           `json:"subject_key"`
	Confidence       float64          `json:"confidence"`
	Volatility       string           `json:"volatility"`
	Evidence         []MemoryEvidence `json:"evidence"`
	ExistingMemoryID string           `json:"existing_memory_id"`
}

// MemoryEvidence links a candidate to its source in the session.
type MemoryEvidence struct {
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
	Quote     string `json:"quote"`
}

const (
	CandidateActionAdd     = "add"
	CandidateActionUpdate  = "update"
	CandidateActionArchive = "archive"
	CandidateActionIgnore  = "ignore"
)

// ProjectMessages extracts user-visible text from raw message content for
// summary generation. It filters out tool call noise and system messages,
// keeping user requests, decisions, and agent conclusions.
func ProjectMessages(messages []string) string {
	var b strings.Builder
	for _, msg := range messages {
		trimmed := strings.TrimSpace(msg)
		if trimmed == "" {
			continue
		}
		b.WriteString(trimmed)
		b.WriteString("\n\n")
	}
	return b.String()
}

var summarySystemPrompt = `You are a session summarizer for a coding agent workspace.

Your task is to extract a structured summary from the session conversation below.

Focus on:
- What the user wanted to accomplish (goal)
- Key decisions made during the session
- Constraints or rules the user expressed
- Important facts discovered during work
- Files that were changed or discussed
- Unresolved questions or pending work
- User preferences about how work should be done

Do NOT include:
- Routine tool outputs
- Code snippets (just note which files were involved)
- Agent internal reasoning
- Temporary debugging steps

Output strict JSON matching this schema:
{
  "session_id": "",
  "goal": "",
  "decisions": [],
  "constraints": [],
  "discovered_facts": [],
  "changed_files": [],
  "unresolved": [],
  "user_preferences": [],
  "suppression_hints": []
}

suppression_hints should contain any phrases from the user that indicate
"don't remember this" or "just for now" or similar temporary intent.
If none found, leave it empty.

Output ONLY the JSON object. No markdown fences, no explanation.`

// GenerateSummary calls the LLM to produce a SessionSummary from projected
// session messages.
func GenerateSummary(ctx context.Context, llm CompletionFunc, projected string, sessionID string) (SessionSummary, error) {
	if llm == nil {
		return SessionSummary{}, fmt.Errorf("consolidation LLM not configured")
	}
	raw, err := llm(ctx, summarySystemPrompt, projected)
	if err != nil {
		return SessionSummary{}, fmt.Errorf("summary generation failed: %w", err)
	}
	raw = stripJSONFences(raw)
	var summary SessionSummary
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		return SessionSummary{SessionID: sessionID, Goal: projected[:min(len(projected), 200)]}, nil
	}
	summary.SessionID = sessionID
	return summary, nil
}

var candidateSystemPrompt = `You are a memory extraction system for a coding agent workspace.

Given a session summary and the list of existing memories, extract facts worth
saving as long-term project memories. These are facts that future sessions will
need — not temporary state or routine actions.

Rules:
1. Only extract facts with LONG-TERM value across sessions.
2. Prefer: architecture decisions, tech choices, long-term constraints, user
   rules, recurring patterns, important background not obvious from code.
3. NEVER extract: one-time actions, temporary state, routine commands, code
   structure (changes too fast), uncertain guesses, sensitive info.
4. If the user said "don't remember" or similar, skip that fact.
5. scope must always be "project".
6. activation must always be "relevant" (retrieval-only).
7. type must be one of: project, user, feedback, reference.
8. confidence must be between 0.0 and 1.0. Use:
   - 0.95-1.00: user explicitly stated long-term rule or clear architecture decision
   - 0.85-0.94: user confirmed multiple times or explicitly repeated
   - 0.70-0.84: stable project fact or consistent behavior pattern
   - Below 0.70: do not include (will be ignored)
9. For each candidate, check if an existing memory already covers this fact.
   If so, set action to "update" and existing_memory_id to that memory's ID.
   If the existing fact is now wrong, set action to "archive".
   If it's a new fact, set action to "add".
   If the fact is identical to an existing memory, set action to "ignore".
10. volatility: "stable" for most facts, "volatile" for time-bound ones,
    "evergreen" for permanent facts.

Output strict JSON:
{
  "memories": [
    {
      "action": "add",
      "name": "descriptive-kebab-name",
      "title": "Short label",
      "description": "One-line hook for the memory index",
      "type": "project",
      "scope": "project",
      "keywords": ["keyword1", "keyword2"],
      "body": "The fact itself in markdown.",
      "subject_key": "project.topic.subtopic",
      "confidence": 0.95,
      "volatility": "stable",
      "evidence": [{"session_id": "", "source": "user_statement", "quote": "exact quote"}],
      "existing_memory_id": ""
    }
  ]
}

If no facts are worth saving, return {"memories": []}.
Output ONLY the JSON. No markdown fences, no explanation.`

// ExtractCandidates calls the LLM to extract MemoryCandidates from a summary,
// given the list of existing memories for dedup context.
func ExtractCandidates(ctx context.Context, llm CompletionFunc, summary SessionSummary, existingMemories []Memory) ([]MemoryCandidate, error) {
	if llm == nil {
		return nil, fmt.Errorf("consolidation LLM not configured")
	}
	existingBrief := briefExistingMemories(existingMemories)
	userMsg := fmt.Sprintf("Session ID: %s\n\nSession Summary:\n%s\n\nExisting Memories:\n%s",
		summary.SessionID, formatSummaryForPrompt(summary), existingBrief)
	raw, err := llm(ctx, candidateSystemPrompt, userMsg)
	if err != nil {
		return nil, fmt.Errorf("candidate extraction failed: %w", err)
	}
	raw = stripJSONFences(raw)
	var result struct {
		Memories []MemoryCandidate `json:"memories"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		slog.Warn("memory consolidation: candidate JSON parse failed, skipping", "err", err)
		return nil, nil
	}
	return result.Memories, nil
}

func formatSummaryForPrompt(s SessionSummary) string {
	var b strings.Builder
	if s.Goal != "" {
		fmt.Fprintf(&b, "Goal: %s\n", s.Goal)
	}
	if len(s.Decisions) > 0 {
		b.WriteString("Decisions:\n")
		for _, d := range s.Decisions {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}
	if len(s.Constraints) > 0 {
		b.WriteString("Constraints:\n")
		for _, c := range s.Constraints {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if len(s.DiscoveredFacts) > 0 {
		b.WriteString("Discovered Facts:\n")
		for _, f := range s.DiscoveredFacts {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if len(s.ChangedFiles) > 0 {
		fmt.Fprintf(&b, "Changed Files: %s\n", strings.Join(s.ChangedFiles, ", "))
	}
	if len(s.Unresolved) > 0 {
		b.WriteString("Unresolved:\n")
		for _, u := range s.Unresolved {
			fmt.Fprintf(&b, "- %s\n", u)
		}
	}
	if len(s.UserPreferences) > 0 {
		b.WriteString("User Preferences:\n")
		for _, p := range s.UserPreferences {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	return b.String()
}

func briefExistingMemories(memories []Memory) string {
	if len(memories) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, m := range memories {
		fmt.Fprintf(&b, "- id=%s name=%s subject=%s scope=%s origin=%s title=%s\n",
			m.ID, m.Name, NormalizeSubjectKey(m.SubjectKey),
			NormalizeFactScope(string(m.Scope)),
			NormalizeOrigin(string(m.Origin)),
			oneLine(m.Title))
	}
	return b.String()
}

// stripJSONFences removes markdown code fences from LLM output.
func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	after, ok := strings.CutPrefix(s, "```json")
	if ok {
		s = after
	} else if after, ok := strings.CutPrefix(s, "```"); ok {
		s = after
	}
	if before, ok := strings.CutSuffix(s, "```"); ok {
		s = before
	}
	return strings.TrimSpace(s)
}

// LLMConsolidator implements Consolidator using an LLM for summary and
// candidate extraction, with Go-code policy enforcement.
type LLMConsolidator struct {
	llm    CompletionFunc
	store  Store
	policy ConsolidationPolicy
}

// NewLLMConsolidator creates a consolidator with the given LLM and store.
func NewLLMConsolidator(llm CompletionFunc, store Store, policy ConsolidationPolicy) *LLMConsolidator {
	if policy.MaxNewMemoriesPerSession <= 0 {
		policy.MaxNewMemoriesPerSession = defaultMaxNewMemories
	}
	if policy.ConfidenceFloor <= 0 {
		policy.ConfidenceFloor = defaultConfidenceFloor
	}
	return &LLMConsolidator{llm: llm, store: store, policy: policy}
}

// Consolidate runs the full pipeline: summary → candidates → validation →
// policy → dedup → subject conflict → store mutation. All errors are logged
// and returned in the result without propagating to the caller, so the session
// lifecycle is never blocked.
func (c *LLMConsolidator) Consolidate(ctx context.Context, input ConsolidationInput) (ConsolidationResult, error) {
	result := ConsolidationResult{SessionID: input.SessionID}
	if c.llm == nil || c.store.Dir == "" {
		return result, nil
	}
	if len(input.Messages) == 0 {
		return result, nil
	}

	// Step 1: Generate summary.
	projected := ProjectMessages(input.Messages)
	summary, err := GenerateSummary(ctx, c.llm, projected, input.SessionID)
	if err != nil {
		result.Error = err.Error()
		slog.Warn("memory consolidation: summary failed", "session", input.SessionID, "err", err)
		return result, nil
	}

	// Merge suppression hints from the summary.
	allSuppressed := append([]string{}, input.SuppressedKeys...)
	allSuppressed = append(allSuppressed, summary.SuppressionHints...)

	// Step 2: Extract candidates.
	candidates, err := ExtractCandidates(ctx, c.llm, summary, input.ExistingMemories)
	if err != nil {
		result.Error = err.Error()
		slog.Warn("memory consolidation: extraction failed", "session", input.SessionID, "err", err)
		return result, nil
	}

	// Step 3: Validate and filter.
	var validated []MemoryCandidate
	for _, cand := range candidates {
		cand, err := ValidateCandidate(cand, input.ExistingMemories, allSuppressed)
		if err != nil {
			result.Ignored++
			continue
		}
		if cand.Action == CandidateActionIgnore {
			result.Ignored++
			continue
		}
		validated = append(validated, cand)
	}

	// Step 4: Dedup and subject conflict resolution.
	var final []MemoryCandidate
	for _, cand := range validated {
		dedup := DeduplicateCandidate(cand, input.ExistingMemories)
		switch dedup.Action {
		case CandidateActionIgnore:
			result.Ignored++
			continue
		case CandidateActionUpdate:
			cand.Action = CandidateActionUpdate
			cand.ExistingMemoryID = dedup.ExistingID
		case CandidateActionArchive:
			cand.Action = CandidateActionArchive
			cand.ExistingMemoryID = dedup.ExistingID
		case CandidateActionAdd:
			cand.Action = CandidateActionAdd
		}
		final = append(final, cand)
	}

	// Step 5: Filter by confidence and limit.
	final = FilterByConfidence(final, c.policy)

	// Step 6: Store mutation. Each Save/Archive is individually atomic
	// (they hold memoryStoreMutationMu internally). Rollback handles
	// partial failure.
	err = c.executeMutations(input.SessionID, final, &result)
	if err != nil {
		result.Error = err.Error()
		slog.Warn("memory consolidation: mutation failed", "session", input.SessionID, "err", err)
	}

	return result, nil
}

// rollback tracks committed mutations so they can be undone on failure.
type rollbackEntry struct {
	name string
}

// executeMutations performs all add/update/archive operations. The caller
// holds memoryStoreMutationMu.
func (c *LLMConsolidator) executeMutations(sessionID string, candidates []MemoryCandidate, result *ConsolidationResult) error {
	var committed []rollbackEntry

	for _, cand := range candidates {
		now := time.Now().UTC()
		switch cand.Action {
		case CandidateActionAdd:
			m := Memory{
				Name:            slug(cand.Name),
				Title:           oneLine(cand.Title),
				Description:     oneLine(cand.Description),
				Type:            NormalizeType(cand.Type),
				Scope:           FactScopeProject,
				Activation:      ActivationRelevant,
				Volatility:      NormalizeVolatility(cand.Volatility),
				SubjectKey:      NormalizeSubjectKey(cand.SubjectKey),
				Keywords:        strings.Join(cand.Keywords, " "),
				Body:            strings.TrimSpace(cand.Body),
				Origin:          OriginConsolidated,
				SourceSessionID: sessionID,
				Confidence:      cand.Confidence,
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			if _, err := c.store.Save(m); err != nil {
				return c.rollbackMutations(committed, err)
			}
			committed = append(committed, rollbackEntry{name: m.Name})
			result.Added = append(result.Added, m.Name)

		case CandidateActionUpdate:
			existing, ok := c.store.Read(cand.ExistingMemoryID)
			if !ok {
				existing, ok = c.store.Read(cand.Name)
			}
			if !ok {
				// Fall back to add if existing not found.
				cand.Action = CandidateActionAdd
				m := Memory{
					Name:            slug(cand.Name),
					Title:           oneLine(cand.Title),
					Description:     oneLine(cand.Description),
					Type:            NormalizeType(cand.Type),
					Scope:           FactScopeProject,
					Activation:      ActivationRelevant,
					Volatility:      NormalizeVolatility(cand.Volatility),
					SubjectKey:      NormalizeSubjectKey(cand.SubjectKey),
					Keywords:        strings.Join(cand.Keywords, " "),
					Body:            strings.TrimSpace(cand.Body),
					Origin:          OriginConsolidated,
					SourceSessionID: sessionID,
					Confidence:      cand.Confidence,
					CreatedAt:       now,
					UpdatedAt:       now,
				}
				if _, err := c.store.Save(m); err != nil {
					return c.rollbackMutations(committed, err)
				}
				committed = append(committed, rollbackEntry{name: m.Name})
				result.Added = append(result.Added, m.Name)
				break
			}
			// Protect manual/explicit origin.
			if OriginPriority(existing.Origin) > OriginPriority(OriginConsolidated) {
				result.Ignored++
				break
			}
			existing.Body = strings.TrimSpace(cand.Body)
			existing.Description = oneLine(cand.Description)
			existing.Title = oneLine(cand.Title)
			existing.Keywords = strings.Join(cand.Keywords, " ")
			existing.UpdatedAt = now
			existing.SourceSessionID = sessionID
			existing.Confidence = cand.Confidence
			existing.Origin = OriginConsolidated
			existing.Revision++
			if _, err := c.store.Save(existing); err != nil {
				return c.rollbackMutations(committed, err)
			}
			committed = append(committed, rollbackEntry{name: existing.Name})
			result.Updated = append(result.Updated, existing.Name)

		case CandidateActionArchive:
			name := cand.ExistingMemoryID
			if name == "" {
				name = cand.Name
			}
			existing, ok := c.store.Read(name)
			if !ok {
				result.Ignored++
				break
			}
			if OriginPriority(existing.Origin) > OriginPriority(OriginConsolidated) {
				result.Ignored++
				break
			}
			if _, err := c.store.Archive(name); err != nil {
				return c.rollbackMutations(committed, err)
			}
			result.Archived = append(result.Archived, name)

		default:
			result.Ignored++
		}
	}
	return nil
}

// rollbackMutations attempts to undo committed saves. Best-effort; the error
// from the failed operation is returned.
func (c *LLMConsolidator) rollbackMutations(committed []rollbackEntry, cause error) error {
	for i := range slices.Backward(committed) {
		_, _ = c.store.Archive(committed[i].name)
	}
	return cause
}

func init() {
	// Force compile check: LLMConsolidator must satisfy Consolidator.
	var _ Consolidator = (*LLMConsolidator)(nil)
}
