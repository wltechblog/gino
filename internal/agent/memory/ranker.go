package memory

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Ranker ranks memory items relative to a query and returns the top N items.
type Ranker interface {
	Rank(query string, memories []MemoryItem, top int) []MemoryItem
}

// SimpleRanker scores memories by keyword overlap with the query.
// It's intentionally simple and deterministic for testing.
type SimpleRanker struct{}

func NewSimpleRanker() *SimpleRanker { return &SimpleRanker{} }

// ErrNoIndicesFound returned when no JSON-like indices could be parsed from provider text.
var ErrNoIndicesFound = fmt.Errorf("no indices found in response")

// tokenize extracts lowercase word tokens of length >= 2.
func tokenize(s string) []string {
	re := regexp.MustCompile(`\w+`)
	parts := re.FindAllString(s, -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(p)
		if len(p) >= 2 {
			out = append(out, p)
		}
	}
	return out
}

func (s *SimpleRanker) Rank(query string, memories []MemoryItem, top int) []MemoryItem {
	if top <= 0 || top > len(memories) {
		top = len(memories)
	}
	qTokens := tokenize(query)
	if len(qTokens) == 0 {
		// no query tokens; return the most recent items (as stored: newer at the end)
		out := make([]MemoryItem, len(memories))
		copy(out, memories)
		// reverse for most-recent-first
		rev := make([]MemoryItem, 0, len(out))
		for i := len(out) - 1; i >= 0; i-- {
			rev = append(rev, out[i])
		}
		if top < len(rev) {
			return rev[:top]
		}
		return rev
	}

	type scored struct {
		m     MemoryItem
		score int
		idx   int
	}

	scores := make([]scored, 0, len(memories))
	for i, m := range memories {
		mTokens := tokenize(m.Text)
		set := make(map[string]struct{}, len(mTokens))
		for _, t := range mTokens {
			set[t] = struct{}{}
		}
		score := 0
		for _, qt := range qTokens {
			if _, ok := set[qt]; ok {
				score++
			}
		}
		scores = append(scores, scored{m: m, score: score, idx: i})
	}

	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score != scores[j].score {
			return scores[i].score > scores[j].score
		}
		// tiebreaker: more recent (higher index => newer)
		return scores[i].idx > scores[j].idx
	})

	out := make([]MemoryItem, 0, top)
	for i := 0; i < top && i < len(scores); i++ {
		out = append(out, scores[i].m)
	}
	return out
}

// FilterByFloor drops items whose position-derived relevance score falls
// below floor, using the same reciprocal-rank scale as brain search:
// score(i) = 1/(i+10) for 0-based position i (rank 0 → 0.1, rank 9 → 0.05).
// MemoryItem carries no intrinsic score, so position in the ranked list is
// the only relevance signal available; the floor therefore bounds how deep
// into the ranked list injection reaches (0.01 = everything within top-K,
// 0.095 = only the single top item). floor <= 0 disables filtering —
// unset/0 keeps legacy behavior (all ranked items inject). An item exactly
// ON the floor injects (>= semantics; note this is the opposite boundary of
// brain applyMinScore's strictly-greater rule — each is documented where it
// lives). Wired from agents.defaults.sessionCompaction.memoryMinScore via
// ContextBuilder.SetMemoryFloor.
func FilterByFloor(items []MemoryItem, floor float64) []MemoryItem {
	if floor <= 0 || len(items) == 0 {
		return items
	}
	out := make([]MemoryItem, 0, len(items))
	for i, m := range items {
		if 1.0/float64(i+10) >= floor {
			out = append(out, m)
		}
	}
	return out
}
