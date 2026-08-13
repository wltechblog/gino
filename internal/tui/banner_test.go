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

func sgrBoldDim(s string) (bold, dim bool) {
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == 0x1b && s[i+1] == '[' {
			i += 2
			start := i
			for i < len(s) {
				if s[i] >= '@' && s[i] <= '~' {
					if s[i] == 'm' {
						params := s[start:i]
						if params == "" {
							params = "0"
						}
						for _, p := range strings.Split(params, ";") {
							switch p {
							case "", "0":
								bold, dim = false, false
							case "1":
								bold = true
							case "2":
								dim = true
							case "22":
								bold, dim = false, false
							}
						}
					}
					i++
					break
				}
				i++
			}
			continue
		}
		i++
	}
	return bold, dim
}

func TestFormatBannerClosesInnerStylesBeforeBorders(t *testing.T) {
	lines := formatBanner("v0.5.0", "qwen3.5:4b")
	assertUniformBanner(t, lines)

	title := lines[1]
	if _, dim := sgrBoldDim(title[:strings.LastIndex(title, "║")]); dim {
		t.Fatalf("title row left dim active at closing border: %q", title)
	}

	help := lines[3]
	border := strings.LastIndex(help, "║")
	if border < 0 {
		t.Fatalf("help row missing closing border: %q", help)
	}
	if bold, _ := sgrBoldDim(help[:border]); bold {
		t.Fatalf("help row left bold active at closing border: %q", help)
	}

	throughHelp := strings.Join(lines[:4], "\n")
	if bold, _ := sgrBoldDim(throughHelp); bold {
		t.Fatalf("bold leaked from help row onto later output: %q", throughHelp)
	}

	joined := strings.Join(lines, "\n")
	if bold, dim := sgrBoldDim(joined); bold || dim {
		t.Fatalf("banner left intensity attributes enabled (bold=%v dim=%v)", bold, dim)
	}
}
