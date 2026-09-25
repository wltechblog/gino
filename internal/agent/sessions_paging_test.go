package agent

import (
	"context"
	"encoding/json"
"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
		"github.com/wltechblog/gino/internal/session"
)

// TestBuildSessionKeyboardPagination checks the paginated keyboard: global
// numbering, 10 rows max, and a nav row on multi-page lists.
func TestBuildSessionKeyboardPagination(t *testing.T) {
	var sessions []*session.Session
	for i := 0; i < 23; i++ {
		sessions = append(sessions, &session.Session{
			Key:       "telegram:1:archive:100",
			Title:     "Session " + string(rune('A'+i)),
			UpdatedAt: time.Now(),
		})
	}

	// Page 1: rows 1-10, nav present.
	markup := buildSessionKeyboard(sessions, 1)
	var kb struct {
		InlineKeyboard [][]struct {
			Text         string `json:"text"`
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(markup), &kb); err != nil {
		t.Fatalf("invalid keyboard JSON: %v", err)
	}
	if len(kb.InlineKeyboard) != 11 { // 10 sessions + 1 nav
		t.Fatalf("page 1: expected 11 rows, got %d", len(kb.InlineKeyboard))
	}
	if got := kb.InlineKeyboard[0][0].CallbackData; got != "sw:1" {
		t.Fatalf("first row callback = %q, want sw:1", got)
	}
	if got := kb.InlineKeyboard[9][0].CallbackData; got != "sw:10" {
		t.Fatalf("row 10 callback = %q, want sw:10", got)
	}
	nav := kb.InlineKeyboard[10]
	if len(nav) != 2 { // page 1 has no prev
		t.Fatalf("page 1 nav row: expected 2 buttons, got %d", len(nav))
	}
	if !strings.Contains(nav[1].Text, "▶") {
		t.Fatalf("page 1 nav: expected next button, got %+v", nav)
	}
	if nav[1].CallbackData != "swpg:2" {
		t.Fatalf("next button callback = %q, want swpg:2", nav[1].CallbackData)
	}

	// Page 2: rows 11-20, both prev and next.
	markup2 := buildSessionKeyboard(sessions, 2)
	if err := json.Unmarshal([]byte(markup2), &kb); err != nil {
		t.Fatalf("invalid keyboard JSON: %v", err)
	}
	if len(kb.InlineKeyboard) != 11 {
		t.Fatalf("page 2: expected 11 rows, got %d", len(kb.InlineKeyboard))
	}
	if got := kb.InlineKeyboard[0][0].CallbackData; got != "sw:11" {
		t.Fatalf("page 2 first row callback = %q, want sw:11", got)
	}
	nav2 := kb.InlineKeyboard[10]
	if len(nav2) != 3 {
		t.Fatalf("page 2 nav row: expected 3 buttons, got %d", len(nav2))
	}
	if nav2[0].CallbackData != "swpg:1" || nav2[2].CallbackData != "swpg:3" {
		t.Fatalf("page 2 nav callbacks = %q,%q want swpg:1,swpg:3", nav2[0].CallbackData, nav2[2].CallbackData)
	}

	// Page 3: rows 21-23 (3 rows), prev only.
	markup3 := buildSessionKeyboard(sessions, 3)
	if err := json.Unmarshal([]byte(markup3), &kb); err != nil {
		t.Fatalf("invalid keyboard JSON: %v", err)
	}
	if len(kb.InlineKeyboard) != 4 {
		t.Fatalf("page 3: expected 4 rows, got %d", len(kb.InlineKeyboard))
	}
	if got := kb.InlineKeyboard[2][0].CallbackData; got != "sw:23" {
		t.Fatalf("page 3 last row callback = %q, want sw:23", got)
	}
	nav3 := kb.InlineKeyboard[3]
	if len(nav3) != 2 {
		t.Fatalf("page 3 nav row: expected 2 buttons, got %d", len(nav3))
	}
	if nav3[0].CallbackData != "swpg:2" {
		t.Fatalf("page 3 prev callback = %q, want swpg:2", nav3[0].CallbackData)
	}

	// Single page: no nav row.
	few := sessions[:5]
	markup4 := buildSessionKeyboard(few, 1)
	if err := json.Unmarshal([]byte(markup4), &kb); err != nil {
		t.Fatalf("invalid keyboard JSON: %v", err)
	}
	if len(kb.InlineKeyboard) != 5 {
		t.Fatalf("single page: expected 5 rows, got %d", len(kb.InlineKeyboard))
	}

	// Out-of-range page clamps.
	markup5 := buildSessionKeyboard(sessions, 99)
	if err := json.Unmarshal([]byte(markup5), &kb); err != nil {
		t.Fatalf("invalid keyboard JSON: %v", err)
	}
	if got := kb.InlineKeyboard[0][0].CallbackData; got != "sw:21" {
		t.Fatalf("clamped page first row callback = %q, want sw:21", got)
	}
}

// TestParseSessionsPage covers the /sessions <page> argument parsing.
func TestParseSessionsPage(t *testing.T) {
	if p, explicit := parseSessionsPage(""); p != 1 || explicit {
		t.Fatalf("empty: got %d,%v want 1,false", p, explicit)
	}
	if p, explicit := parseSessionsPage(" 3 "); p != 3 || !explicit {
		t.Fatalf("3: got %d,%v want 3,true", p, explicit)
	}
	if p, explicit := parseSessionsPage(" abc "); p != 1 || explicit {
		t.Fatalf("abc: got %d,%v want 1,false", p, explicit)
	}
	if p, explicit := parseSessionsPage(" 0 "); p != 1 || explicit {
		t.Fatalf("0: got %d,%v want 1,false", p, explicit)
	}
	if p, explicit := parseSessionsPage(" -2 "); p != 1 || explicit {
		t.Fatalf("-2: got %d,%v want 1,false", p, explicit)
	}
}

// TestSessionsPageCommandAndCallback drives the hub round-trip: /sessions 2
// on Telegram must send a paginated keyboard, and a swpg:3 callback must
// re-render with page 3 rows.
func TestSessionsPageCommandAndCallback(t *testing.T) {
	ag, _, b := newTitleTestLoop(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go ag.Run(ctx)

	key := "telegram:456"
	s := ag.sessions.GetOrCreate(key)
	s.History = []string{"user: hello", "assistant: hi"}

	// 23 archived sessions -> 3 pages.
	base := time.Now()
	for i := 0; i < 23; i++ {
		ak := key + ":archive:" + strconv.FormatInt(base.UnixNano()-int64(i)*1000, 10)
		as := ag.sessions.GetOrCreate(ak)
		as.Title = "Archive " + strconv.Itoa(i+1)
		as.History = []string{"user: x"}
	}

	send := func(content string, meta map[string]interface{}) string {
		t.Helper()
		select {
		case b.In <- chat.Inbound{Channel: "telegram", SenderID: "u", ChatID: "456", Content: content, Metadata: meta}:
		default:
			t.Fatal("couldn't send message")
		}
		deadline := time.After(3 * time.Second)
		for {
			select {
			case out := <-b.Out:
				return out.Content
			case <-deadline:
				t.Fatal("no reply for " + content)
			}
		}
	}

	// /sessions 2 -> text includes page 2/3.
	out := send("/sessions 2", map[string]interface{}{"privileged": true})
	if !strings.Contains(out, "2/3") {
		t.Fatalf("/sessions 2 output missing page hint: %q", out)
	}

	// swpg:3 callback re-renders the keyboard listing page 3 (sessions 21-23).
	out = send("swpg:3", map[string]interface{}{"privileged": true, "callback_data": "swpg:3"})
	if !strings.Contains(out, "3/3") {
		t.Fatalf("swpg:3 output missing page hint: %q", out)
	}

	// Plain /sessions still works (page 1).
	out = send("/sessions", map[string]interface{}{"privileged": true})
	if !strings.Contains(out, "Current") {
		t.Fatalf("/sessions output missing Current header: %q", out)
	}
}
