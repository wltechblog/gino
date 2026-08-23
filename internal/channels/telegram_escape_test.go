//go:build !only_discord && !only_slack && !only_whatsapp

package channels

import (
	"strings"
	"testing"
)

// Regression: the exact production failure from 2026-08-23 — an LLM reply
// containing standard-markdown **bold** followed by a fenced code block.
// The old escaper had no ** branch: the first * was dropped as \* and the
// second opened a bogus bold span that swallowed the opening ``` (escaping
// its backticks individually), leaving a raw closing fence = unterminated
// Pre entity → Telegram 400 "Can't find end of Pre entity".
func TestEscapeReservedStandardBoldPlusCodeFence(t *testing.T) {
	in := "Done — eb4a203 pushed.\n\n**The change:** use `json.Marshal` instead:\n\n```\ndocker logs gino 2>&1 | grep 'LLM USAGE' | jq -r '.usage'\n```\n\nEnd."
	out := tgEscapeReserved(stripLLMEscapes(in))

	if !strings.Contains(out, "*The change:*") {
		t.Errorf("**bold** not converted to Telegram bold:\n%s", out)
	}
	// Both fences must survive as raw ``` pairs (4 backticks total here).
	if got := strings.Count(out, "```"); got != 2 {
		t.Errorf("expected 2 code fences (open+close), got %d:\n%s", got, out)
	}
	// Fence must not be split or individually escaped.
	if strings.Contains(out, "\\`\\`\\`") {
		t.Errorf("fence backticks individually escaped (unclosed Pre entity):\n%s", out)
	}
	// The pipe and quotes inside the block must remain verbatim (code blocks
	// are exempt from escaping).
	if !strings.Contains(out, "docker logs gino 2>&1 | grep") {
		t.Errorf("code block content mangled:\n%s", out)
	}
}

// Regression: a lone unterminated fence must be escaped as literal text,
// never emitted raw (raw = unclosed Pre entity = Telegram 400).
func TestEscapeReservedUnterminatedFence(t *testing.T) {
	out := tgEscapeReserved(stripLLMEscapes("Start\n```\ndocker logs gino | grep USAGE\nnever closed"))
	if strings.Contains(out, "```") {
		t.Errorf("unterminated fence emitted raw — Telegram would reject:\n%s", out)
	}
	if !strings.Contains(out, "\\`\\`\\`") {
		t.Errorf("unterminated fence not escaped as literal:\n%s", out)
	}
}

// Regression: unterminated ** must be escaped as literal, not open a span.
func TestEscapeReservedUnterminatedDoubleBold(t *testing.T) {
	out := tgEscapeReserved(stripLLMEscapes("a **broken bold and `code` too"))
	if strings.Contains(out, "*broken bold and") && !strings.Contains(out, "\\*\\*broken") {
		t.Errorf("unterminated ** opened a span:\n%s", out)
	}
	if !strings.Contains(out, "\\*\\*broken") {
		t.Errorf("unterminated ** not escaped literally:\n%s", out)
	}
	// The paired backticks after the escaped ** form a valid inline code
	// span and must be preserved verbatim (not individually escaped).
	if !strings.Contains(out, "`code`") {
		t.Errorf("paired inline code after broken bold not preserved:\n%s", out)
	}
}

// MarkdownV2 requires literal backslashes as \\. Stray backslashes must not
// be emitted raw (illegal escape sequences) nor silently dropped.
func TestEscapeReservedBackslash(t *testing.T) {
	out := tgEscapeReserved(stripLLMEscapes("path C:\\dir\\file.txt"))
	if !strings.Contains(out, "C:\\\\dir\\\\file") {
		t.Errorf("backslashes not doubled:\n%s", out)
	}
}

// LLM-pre-escaped fences (\`\`\` with backslashes) must be normalized to a
// proper raw fence after stripLLMEscapes + tgEscapeReserved.
func TestEscapeReservedPreEscapedFenceNormalized(t *testing.T) {
	in := "shell\n\\`\\`\\`\nls -la\n\\`\\`\\`\nafter"
	out := tgEscapeReserved(stripLLMEscapes(in))
	if strings.Count(out, "```") != 2 {
		t.Errorf("pre-escaped fences not normalized to raw pair:\n%s", out)
	}
}
