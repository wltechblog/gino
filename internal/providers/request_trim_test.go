package providers

import (
	"log"
	"strings"
	"testing"
)

// restoreTrimCap puts the process-wide trim cap back to the default after a
// test that mutated it. Every cap-mutating test must register it — the cap
// is global state shared by the whole test binary.
func restoreTrimCap(t *testing.T) {
	t.Cleanup(func() { SetMaxRequestChars(DefaultMaxRequestChars) })
}

func TestEmergencyTrimTrimsFatHistoryPreservingSystem(t *testing.T) {
	sys := Message{Role: "system", Content: strings.Repeat("S", 5000)}
	fat := Message{Role: "user", Content: strings.Repeat("F", 300_000)}
	recent := Message{Role: "user", Content: strings.Repeat("R", 2000)}
	msgs := []Message{sys, fat, recent}

	out := emergencyTrimMessages(msgs)

	total := 0
	for i := range out {
		total += msgChars(&out[i])
	}
	if total > DefaultMaxRequestChars {
		t.Fatalf("total=%d still over cap %d", total, DefaultMaxRequestChars)
	}
	if out[0].Content != sys.Content {
		t.Fatalf("system message not preserved (len %d)", len(out[0].Content))
	}
	if out[len(out)-1].Content != recent.Content {
		t.Fatalf("recent tail not preserved")
	}
	foundStub := false
	for _, m := range out {
		if strings.Contains(m.Content, "trimmed automatically") {
			foundStub = true
		}
	}
	if !foundStub {
		t.Fatalf("elided-history stub note missing")
	}
}

func TestEmergencyTrimPreservesToolPairing(t *testing.T) {
	sys := Message{Role: "system", Content: strings.Repeat("S", 3000)}
	tc := Message{Role: "assistant", ToolCalls: []ToolCall{{
		ID: "call-1", Name: "web",
		Arguments: map[string]interface{}{"url": "http://x/" + strings.Repeat("a", 500)},
	}}}
	res := Message{Role: "tool", ToolCallID: "call-1", Content: strings.Repeat("T", 100_000)}
	pair2 := Message{Role: "assistant", ToolCalls: []ToolCall{{
		ID: "call-2", Name: "exec",
		Arguments: map[string]interface{}{"cmd": "ls"},
	}}}
	res2 := Message{Role: "tool", ToolCallID: "call-2", Content: strings.Repeat("U", 150_000)}
	tail := Message{Role: "user", Content: "go on"}
	msgs := []Message{sys, tc, res, pair2, res2, tail}

	out := emergencyTrimMessages(msgs)
	// Every tool result must still be preceded by its calling assistant.
	for i, m := range out {
		if m.Role == "tool" {
			ok := false
			for j := i - 1; j >= 0; j-- {
				if out[j].Role == "assistant" && len(out[j].ToolCalls) > 0 {
					for _, c := range out[j].ToolCalls {
						if c.ID == m.ToolCallID {
							ok = true
						}
					}
					break
				}
			}
			if !ok {
				t.Fatalf("orphaned tool result id=%s", m.ToolCallID)
			}
		}
	}
	total := 0
	for i := range out {
		total += msgChars(&out[i])
	}
	if total > DefaultMaxRequestChars {
		t.Fatalf("total=%d over cap", total)
	}
}

func TestEmergencyTrimNoopOnHealthyRequest(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: strings.Repeat("S", 1000)},
		{Role: "user", Content: strings.Repeat("U", 5000)},
	}
	orig := make([]string, len(msgs))
	for i, m := range msgs {
		orig[i] = m.Content
	}
	out := emergencyTrimMessages(msgs)
	if len(out) != len(msgs) {
		t.Fatalf("healthy request changed message count: %d -> %d", len(msgs), len(out))
	}
	for i := range out {
		if out[i].Content != orig[i] {
			t.Fatalf("healthy request message %d modified", i)
		}
	}
}

func TestEmergencyTrimClampsSingleHugeTail(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: strings.Repeat("S", 1000)},
		{Role: "user", Content: strings.Repeat("X", 400_000)},
	}
	out := emergencyTrimMessages(msgs)
	total := 0
	for i := range out {
		total += msgChars(&out[i])
	}
	if total > DefaultMaxRequestChars {
		t.Fatalf("single-huge-tail total=%d over cap", total)
	}
	if len(out[1].Content) >= 400_000 {
		t.Fatalf("fat tail not clamped (len=%d)", len(out[1].Content))
	}
}

func TestEmergencyTrimStripsOversizedImages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: strings.Repeat("S", 1000)},
		{Role: "user", Content: "look at this", Images: []string{"data:image/png;base64," + strings.Repeat("A", 400_000)}},
	}
	out := emergencyTrimMessages(msgs)
	total := 0
	imgLeft := 0
	for i := range out {
		total += msgChars(&out[i])
		imgLeft += len(out[i].Images)
	}
	if total > DefaultMaxRequestChars {
		t.Fatalf("image-bloat total=%d over cap", total)
	}
	if imgLeft != 0 {
		t.Fatalf("oversized images not stripped (%d bytes left)", imgLeft)
	}
}

// ---- GINO-RAILS-006 item 1: trim observability ----

// captureTrimLogs swaps the log package's default output for a buffer so
// tests can assert the trim fired a visibility line.
func captureTrimLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(log.Writer()) })
	return &buf
}

// TestEmergencyTrimLogsEveryFiring pins the #1 GINO-RAILS-006 fix: the trim
// used to fire silently, so hosts degraded for days with no signal. Every
// firing MUST produce a log line with before/after sizes and keep-window
// positions.
func TestEmergencyTrimLogsEveryFiring(t *testing.T) {
	restoreTrimCap(t)
	buf := captureTrimLogs(t)

	before := EmergencyTrimCount()
	msgs := []Message{
		{Role: "system", Content: strings.Repeat("S", 5000)},
		{Role: "user", Content: strings.Repeat("F", 300_000)},
		{Role: "assistant", Content: strings.Repeat("A", 2000)},
		{Role: "user", Content: strings.Repeat("R", 2000)},
	}
	_ = emergencyTrimMessages(msgs)

	out := buf.String()
	if !strings.Contains(out, "[request-trim]") {
		t.Fatalf("trim fired without visibility log line: %q", out)
	}
	for _, want := range []string{"chars > cap 240000", "4 ->", "trim #"} { // before/after sizes, msg count, counter
		if !strings.Contains(out, want) {
			t.Fatalf("log line missing %q: %q", want, out)
		}
	}
	if got := EmergencyTrimCount(); got != before+1 {
		t.Fatalf("EmergencyTrimCount: want %d, got %d", before+1, got)
	}
}

// TestEmergencyTrimNoLogOnHealthyRequest: the log line must mark FIRINGS,
// not every request — a healthy request stays quiet and does not bump the
// counter.
func TestEmergencyTrimNoLogOnHealthyRequest(t *testing.T) {
	restoreTrimCap(t)
	buf := captureTrimLogs(t)

	before := EmergencyTrimCount()
	msgs := []Message{
		{Role: "system", Content: strings.Repeat("S", 1000)},
		{Role: "user", Content: strings.Repeat("U", 5000)},
	}
	_ = emergencyTrimMessages(msgs)

	if out := buf.String(); strings.Contains(out, "[request-trim]") {
		t.Fatalf("healthy request logged a trim line: %q", out)
	}
	if got := EmergencyTrimCount(); got != before {
		t.Fatalf("healthy request bumped trim counter: %d -> %d", before, got)
	}
}

// TestEmergencyTrimDisabledByZeroCap pins the disable contract: cap <= 0
// (config maxRequestChars = 0) must make even a hugely oversized request
// pass through untouched — and quiet.
func TestEmergencyTrimDisabledByZeroCap(t *testing.T) {
	restoreTrimCap(t)
	buf := captureTrimLogs(t)

	SetMaxRequestChars(0)
	fat := strings.Repeat("F", 500_000)
	msgs := []Message{
		{Role: "system", Content: strings.Repeat("S", 1000)},
		{Role: "user", Content: fat},
	}
	out := emergencyTrimMessages(msgs)

	if len(out) != 2 || out[1].Content != fat {
		t.Fatalf("disabled trim modified the request (len=%d)", len(out))
	}
	if s := buf.String(); strings.Contains(s, "[request-trim]") {
		t.Fatalf("disabled trim logged a firing: %q", s)
	}
}

// TestSetMaxRequestCharsCustomCap: a positive value becomes the effective
// cap; requests above it trim, requests below it pass.
func TestSetMaxRequestCharsCustomCap(t *testing.T) {
	restoreTrimCap(t)
	captureTrimLogs(t)

	SetMaxRequestChars(10_000)
	msgs := []Message{
		{Role: "system", Content: strings.Repeat("S", 1000)},
		{Role: "user", Content: strings.Repeat("F", 50_000)},
		{Role: "user", Content: strings.Repeat("R", 500)},
	}
	out := emergencyTrimMessages(msgs)
	total := 0
	for i := range out {
		total += msgChars(&out[i])
	}
	if total > 10_000 {
		t.Fatalf("custom cap not honored: total=%d > 10000", total)
	}
	if got := effectiveTrimCap(); got != 10_000 {
		t.Fatalf("effectiveTrimCap: want 10000, got %d", got)
	}
}
