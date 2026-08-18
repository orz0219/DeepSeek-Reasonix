// Relevant memory retrieval for consolidation: extracts keywords from a session
// summary and queries the store for top-K related memories, replacing the
// previous ListAll() full-dump approach.
package memory

import (
	"sort"
	"strings"

	"reasonix/internal/retrieval"
)

const defaultRelatedMemoryLimit = 10

// RetrieveRelatedMemories extracts keywords from the summary and returns the
// top-K most relevant existing memories. This replaces ListAll() in the
// consolidation pipeline so the LLM only sees relevant dedup context.
func RetrieveRelatedMemories(store Store, summary SessionSummary, limit int) []Memory {
	if limit <= 0 {
		limit = defaultRelatedMemoryLimit
	}
	query := buildRetrievalQuery(summary)
	if strings.TrimSpace(query) == "" {
		return nil
	}
	all := store.ListAll()
	if len(all) == 0 {
		return nil
	}
	queryTerms, err := retrieval.QueryTerms(query)
	if err != nil || len(queryTerms) == 0 {
		return nil
	}
	type scored struct {
		memory Memory
		score  float64
	}
	docs := make([]scored, 0, len(all))
	counts := make([]map[string]int, 0, len(all))
	totalLen := 0
	for _, m := range all {
		text := memorySearchText(m)
		terms := retrieval.Tokens(text)
		if len(terms) == 0 {
			continue
		}
		c := retrieval.Counts(terms)
		counts = append(counts, c)
		totalLen += len(terms)
		docs = append(docs, scored{memory: m, score: 0})
	}
	if len(docs) == 0 {
		return nil
	}
	df := retrieval.DocumentFrequency(counts)
	avgLen := float64(totalLen) / float64(len(docs))
	for i := range docs {
		docs[i].score = retrieval.BM25Score(counts[i], len(retrieval.Tokens(memorySearchText(docs[i].memory))), queryTerms, df, len(docs), avgLen)
	}
	sort.Slice(docs, func(i, j int) bool {
		return docs[i].score > docs[j].score
	})
	result := make([]Memory, 0, limit)
	for _, d := range docs {
		if d.score <= 0 {
			break
		}
		result = append(result, d.memory)
		if len(result) >= limit {
			break
		}
	}
	return result
}

func buildRetrievalQuery(s SessionSummary) string {
	var parts []string
	if s.Goal != "" {
		parts = append(parts, s.Goal)
	}
	parts = append(parts, s.Decisions...)
	parts = append(parts, s.Constraints...)
	parts = append(parts, s.DiscoveredFacts...)
	parts = append(parts, s.ChangedFiles...)
	parts = append(parts, s.UserPreferences...)
	return strings.Join(parts, " ")
}
