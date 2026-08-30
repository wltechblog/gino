package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/wltechblog/gino/internal/config"
	"github.com/wltechblog/gino/internal/providers"
)

const (
	scDefaultCompactAt  = 40
	scDefaultKeepRecent = 16
	scSummarizeTimeout  = 120 * time.Second
	scSummaryMaxChars   = 6000
	scEntryHeadBytes    = 1000
)

// sessionCompactor summarizes old persisted session history with the LLM so
// long-running sessions stop re-sending every past message forever.
//
// Unlike in-context compaction (which trims the live LLM message chain
// mid-turn), this operates on the session history that BuildMessages replays
// on the NEXT turn: when a session grows past compactAt entries, everything
// older than the newest keepRecent entries is replaced with a single summary
// entry. This is a one-time rewrite per trigger, not a per-turn re-send —
// history shrinks instead of growing monotonically.
type sessionCompactor struct {
	provider   providers.LLMProvider
	model      string
	compactAt  int
	keepRecent int
}

// newSessionCompactor resolves defaults and returns nil when disabled
// (opt-in via config: agents.defaults.sessionCompaction.enabled).
func newSessionCompactor(provider providers.LLMProvider, model string, cfg config.SessionCompactionConfig) *sessionCompactor {
	if cfg.Enabled == nil || !*cfg.Enabled {
		return nil
	}
	c := &sessionCompactor{
		provider:   provider,
		model:      model,
		compactAt:  cfg.CompactAt,
		keepRecent: cfg.KeepRecent,
	}
	if c.compactAt <= 0 {
		c.compactAt = scDefaultCompactAt
	}
	if c.keepRecent <= 0 {
		c.keepRecent = scDefaultKeepRecent
	}
	if c.keepRecent >= c.compactAt {
		// Nothing to summarize unless the trigger exceeds the preserved tail.
		c.compactAt = c.keepRecent + 8
	}
	return c
}

// maybeCompactSession checks the session length and, if it has grown past
// the trigger, summarizes old history in a background goroutine. Safe to
// call from anywhere; never blocks the calling turn. Concurrent triggers
// are harmless: SessionManager.CompactSession re-checks under its lock and
// aborts if history was already rewritten.
func (a *AgentLoop) maybeCompactSession(sessionKey string) {
	if a.sessComp == nil || sessionKey == "" || a.sessions == nil {
		return
	}
	n, ok := a.sessions.CheckHistoryLength(sessionKey)
	if !ok || n < a.sessComp.compactAt {
		return
	}
	a.bgWG.Add(1)
	go func() {
		defer a.bgWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), scSummarizeTimeout)
		defer cancel()
		start := time.Now()
		summarize := func(old []string) (string, error) {
			return a.sessComp.summarize(ctx, old)
		}
		n, err := a.sessions.CompactSession(sessionKey, a.sessComp.keepRecent, summarize)
		if err != nil {
			log.Printf("session compaction: %s: %v (history left as-is)", sessionKey, err)
			return
		}
		if n > 0 {
			log.Printf("session compaction: %s summarized %d entries in %s", sessionKey, n, time.Since(start).Round(time.Millisecond))
		}
	}()
}

// summarize asks the LLM for a compact digest of the given history entries.
func (c *sessionCompactor) summarize(ctx context.Context, old []string) (string, error) {
	var sb strings.Builder
	sb.WriteString("Summarize this earlier portion of a conversation between a user and an AI assistant (entries are 'role: content'; tool results may be truncated). Capture: what was asked and decided, facts learned, files/commands/URLs involved, and anything unresolved that might matter later. Be concise — at most ~400 words.\n\n")
	for _, e := range old {
		line := e
		if len(line) > scEntryHeadBytes {
			line = line[:scEntryHeadBytes] + " …"
		}
		sb.WriteString(line)
		sb.WriteString("\n\n")
	}
	msgs := []providers.Message{
		{Role: "user", Content: sb.String()},
	}
	resp, err := c.provider.Chat(ctx, msgs, nil, c.model)
	if err != nil {
		return "", fmt.Errorf("summarization LLM call failed: %w", err)
	}
	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return "", fmt.Errorf("empty summary")
	}
	if len(summary) > scSummaryMaxChars {
		summary = summary[:scSummaryMaxChars]
	}
	return summary, nil
}
