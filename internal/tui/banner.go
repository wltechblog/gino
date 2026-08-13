package tui

import (
	"strings"
	"unicode"
)

// bannerInnerWidth is the visible cell width between the vertical box borders.
const bannerInnerWidth = 42

func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) {
				c := s[i]
				i++
				if c >= '@' && c <= '~' {
					break
				}
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func runeDisplayWidth(r rune) int {
	if r == 0 || r == '\u200d' || r == '\ufeff' {
		return 0
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	if r >= 0xFE00 && r <= 0xFE0F {
		return 0
	}
	if isWideRune(r) {
		return 2
	}
	return 1
}

func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F:
		return true
	case r == 0x2329 || r == 0x232A:
		return true
	case r >= 0x2E80 && r <= 0xA4CF && r != 0x303F:
		return true
	case r >= 0xAC00 && r <= 0xD7A3:
		return true
	case r >= 0xF900 && r <= 0xFAFF:
		return true
	case r >= 0xFE10 && r <= 0xFE19:
		return true
	case r >= 0xFE30 && r <= 0xFE6F:
		return true
	case r >= 0xFF00 && r <= 0xFF60:
		return true
	case r >= 0xFFE0 && r <= 0xFFE6:
		return true
	case r >= 0x1F300 && r <= 0x1FAFF:
		return true
	case r >= 0x20000 && r <= 0x3FFFD:
		return true
	default:
		return false
	}
}

func displayWidth(s string) int {
	s = stripANSI(s)
	w := 0
	for _, r := range s {
		w += runeDisplayWidth(r)
	}
	return w
}

func truncateDisplay(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if displayWidth(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runeDisplayWidth(r)
		if w+rw > max-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

func padDisplay(s string, width int) string {
	w := displayWidth(s)
	if w > width {
		s = truncateDisplay(stripANSI(s), width)
		w = displayWidth(s)
	}
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

func bannerLine(content string, width int) string {
	return "║" + padDisplay(content, width) + "║"
}

func styledBannerLine(plain, styled string, width int) string {
	plain = stripANSI(plain)
	w := displayWidth(plain)
	if w > width {
		plain = truncateDisplay(plain, width)
		styled = plain
		w = displayWidth(plain)
	}
	return "║" + styled + strings.Repeat(" ", width-w) + "║"
}

func formatBanner(version, model string) []string {
	modelLabel := "  Model: "
	model = truncateDisplay(model, bannerInnerWidth-displayWidth(modelLabel))

	titlePlain := "  🤖 Gino Chat " + version
	titleStyled := "  🤖 Gino Chat " + dim + version + cyan
	modelPlain := modelLabel + model
	helpPlain := "  Type /help for commands"
	helpStyled := "  Type " + bold + "/help" + cyan + " for commands"

	top := "╔" + strings.Repeat("═", bannerInnerWidth) + "╗"
	bottom := "╚" + strings.Repeat("═", bannerInnerWidth) + "╝"

	return []string{
		cyan + top,
		cyan + styledBannerLine(titlePlain, titleStyled, bannerInnerWidth),
		cyan + bannerLine(modelPlain, bannerInnerWidth),
		cyan + styledBannerLine(helpPlain, helpStyled, bannerInnerWidth),
		cyan + bottom + reset,
	}
}
