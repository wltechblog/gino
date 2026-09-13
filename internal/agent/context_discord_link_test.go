package agent

import (
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/agent/memory"
)

// Discord metadata carries guild_id/channel_id; the context builder must surface
// the full deep link (https://discord.com/channels/<guild>/<channel>) so the
// model can reference threads in replies without guessing IDs.
func TestDiscordDeepLinkInTurnContext(t *testing.T) {
	cb := NewContextBuilder(".", memory.NewSimpleRanker(), 5)
	meta := map[string]interface{}{
		"sender_name": "Josh",
		"guild_id":    "987654321098765432",
		"channel_id":  "111222333444555666",
		"privileged":  true,
	}
	msgs := cb.BuildMessages(nil, "hello", "discord", "1548737605741322373", "42", "", nil, meta)
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("expected last message to be user, got %s", last.Role)
	}
	want := "https://discord.com/channels/987654321098765432/111222333444555666"
	if !strings.Contains(last.Content, want) {
		t.Fatalf("expected deep link %s in turn context, got:\n%s", want, last.Content)
	}
	if !strings.Contains(last.Content, "<turn_context>") {
		t.Fatalf("expected turn_context wrap")
	}
}

// channel_id may be absent (older metadata); fall back to the session chatID
// (the thread ID) so the link still resolves.
func TestDiscordDeepLinkFallsBackToChatID(t *testing.T) {
	cb := NewContextBuilder(".", memory.NewSimpleRanker(), 5)
	meta := map[string]interface{}{
		"guild_id":   "987654321098765432",
		"privileged": true,
	}
	msgs := cb.BuildMessages(nil, "hello", "discord", "1548737605741322373", "42", "", nil, meta)
	last := msgs[len(msgs)-1]
	want := "https://discord.com/channels/987654321098765432/1548737605741322373"
	if !strings.Contains(last.Content, want) {
		t.Fatalf("expected fallback deep link %s, got:\n%s", want, last.Content)
	}
}

// Non-Discord channels and DMs (empty guild_id) must not gain a link.
func TestNoDiscordLinkOutsideGuilds(t *testing.T) {
	cb := NewContextBuilder(".", memory.NewSimpleRanker(), 5)

	// Telegram: metadata present, no discord keys
	msgs := cb.BuildMessages(nil, "hello", "telegram", "123", "42", "", nil,
		map[string]interface{}{"privileged": true})
	if strings.Contains(msgs[len(msgs)-1].Content, "discord.com/channels") {
		t.Fatalf("telegram turn context should not contain a discord link")
	}

	// Discord DM: guild_id empty
	msgs = cb.BuildMessages(nil, "hello", "discord", "123", "42", "", nil,
		map[string]interface{}{"guild_id": "", "channel_id": "123", "is_dm": true})
	if strings.Contains(msgs[len(msgs)-1].Content, "discord.com/channels") {
		t.Fatalf("DM turn context should not contain a discord link")
	}

	// Discord with no metadata at all
	msgs = cb.BuildMessages(nil, "hello", "discord", "123", "42", "", nil, nil)
	if strings.Contains(msgs[len(msgs)-1].Content, "discord.com/channels") {
		t.Fatalf("metadata-less discord turn context should not contain a discord link")
	}
}
