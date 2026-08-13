package tui

import (
	"strings"
	"testing"
)

func visibleWidths(lines []string) []int {
	widths := make([]int, len(lines))
	for i, line := range lines {
		widths[i] = displayWidth(line)
	}
	return widths
}

func assertUniformBanner(t *testing.T, lines []string) {
	t.Helper()
	if len(lines) != 5 {
		t.Fatalf("expected 5 banner lines, got %d: %q", len(lines), lines)
	}
	widths := visibleWidths(lines)
	for i := 1; i < len(widths); i++ {
		if widths[i] != widths[0] {
			t.Fatalf("banner line widths differ: %v\n%s", widths, strings.Join(lines, "\n"))
		}
	}
	if widths[0] != bannerInnerWidth+2 {
		t.Fatalf("banner display width = %d, want %d (inner %d + borders)", widths[0], bannerInnerWidth+2, bannerInnerWidth)
	}
}

func TestFormatBannerEmojiRowMatchesBorders(t *testing.T) {
	assertUniformBanner(t, formatBanner("v0.5.0", "qwen3.5:4b"))
}

func TestFormatBannerASCIIModel(t *testing.T) {
	lines := formatBanner("v0.5.0", "qwen3.5:4b")
	assertUniformBanner(t, lines)
	if !strings.Contains(stripANSI(lines[2]), "qwen3.5:4b") {
		t.Fatalf("model line missing name: %q", lines[2])
	}
}

func TestFormatBannerTruncatesLongModel(t *testing.T) {
	long := strings.Repeat("m", 80)
	lines := formatBanner("v0.5.0", long)
	assertUniformBanner(t, lines)
	visible := stripANSI(lines[2])
	if strings.Contains(visible, long) {
		t.Fatal("long model name should be truncated")
	}
	if !strings.Contains(visible, "…") {
		t.Fatalf("expected ellipsis in truncated model line: %q", visible)
	}
}

func TestFormatBannerWideUnicodeModel(t *testing.T) {
	lines := formatBanner("v0.5.0", "模型名称测试🤖🤖🤖")
	assertUniformBanner(t, lines)
}

func TestBannerLinePadsToWidth(t *testing.T) {
	line := bannerLine("  hello", 20)
	if displayWidth(line) != 22 { // 20 inner + 2 borders
		t.Fatalf("displayWidth=%d line=%q", displayWidth(line), line)
	}
}

func TestDisplayWidthCountsEmojiAsTwo(t *testing.T) {
	if displayWidth("🤖") != 2 {
		t.Fatalf("🤖 width = %d, want 2", displayWidth("🤖"))
	}
	if displayWidth("A") != 1 {
		t.Fatalf("A width = %d, want 1", displayWidth("A"))
	}
	if displayWidth(cyan+"hello"+reset) != 5 {
		t.Fatalf("ANSI-styled hello width = %d, want 5", displayWidth(cyan+"hello"+reset))
	}
}
