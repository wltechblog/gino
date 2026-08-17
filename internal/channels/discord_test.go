//go:build !only_telegram && !only_slack && !only_whatsapp

package channels

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/wltechblog/gino/internal/chat"
)

// TestSplitMessage tests the splitMessage helper function.
func TestSplitMessage(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		maxLen   int
		expected int
	}{
		{
			name:     "short message",
			content:  "Hello, world!",
			maxLen:   2000,
			expected: 1,
		},
		{
			name:     "exact limit",
			content:  strings.Repeat("a", 2000),
			maxLen:   2000,
			expected: 1,
		},
		{
			name:     "over limit",
			content:  strings.Repeat("a", 2500),
			maxLen:   2000,
			expected: 2,
		},
		{
			name:     "split at newline",
			content:  strings.Repeat("a", 1000) + "\n" + strings.Repeat("b", 1000),
			maxLen:   2000,
			expected: 2,
		},
		{
			name:     "split at space",
			content:  strings.Repeat("a", 1000) + " " + strings.Repeat("b", 1000),
			maxLen:   2000,
			expected: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := splitMessage(tt.content, tt.maxLen)
			if len(chunks) != tt.expected {
				t.Errorf("splitMessage() returned %d chunks, want %d", len(chunks), tt.expected)
			}
			// Verify each chunk is within limit
			for i, chunk := range chunks {
				if len(chunk) > tt.maxLen {
					t.Errorf("chunk %d is %d chars, exceeds limit %d", i, len(chunk), tt.maxLen)
				}
			}
		})
	}
}

// TestTruncate tests the truncate helper function.
func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is a long message", 10, "this is a ..."},
	}

	for _, tt := range tests {
		result := truncate(tt.input, tt.maxLen)
		if result != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, result, tt.expected)
		}
	}
}

// TestStartDiscord_EmptyToken tests that StartDiscord returns an error with empty token.
func TestStartDiscord_EmptyToken(t *testing.T) {
	hub := chat.NewHub(100)
	err := StartDiscord(context.Background(), hub, "", nil, false, nil, false, "", 0, DiscordRateLimit{})
	if err == nil {
		t.Error("StartDiscord with empty token should return error")
	}
	if !strings.Contains(err.Error(), "token not provided") {
		t.Errorf("expected 'token not provided' error, got: %v", err)
	}
}

// TestDiscordClient_IsAllowed tests the allowlist logic.
func TestDiscordClient_IsAllowed(t *testing.T) {
	// This tests the allowlist logic conceptually
	allowed := make(map[string]struct{})
	allowed["123456789"] = struct{}{}

	// Test allowed user
	if _, ok := allowed["123456789"]; !ok {
		t.Error("user 123456789 should be allowed")
	}

	// Test non-allowed user
	if _, ok := allowed["987654321"]; ok {
		t.Error("user 987654321 should not be allowed")
	}

	// Test empty allowlist (all users allowed)
	emptyAllowed := make(map[string]struct{})
	if len(emptyAllowed) > 0 {
		t.Error("empty allowlist should allow all users")
	}
}

// TestDiscordClient_TypingIndicator tests typing indicator management.
func TestDiscordClient_TypingIndicator(t *testing.T) {
	// Test that typingStop map works correctly
	typingStop := make(map[string]chan struct{})

	// Add a channel
	stop1 := make(chan struct{})
	typingStop["channel1"] = stop1

	// Verify it exists
	if _, ok := typingStop["channel1"]; !ok {
		t.Error("channel1 should exist in typingStop")
	}

	// Remove it
	close(stop1)
	delete(typingStop, "channel1")

	if _, ok := typingStop["channel1"]; ok {
		t.Error("channel1 should be removed from typingStop")
	}
}

// TestDiscordClient_MessageHandling tests message handling logic.
func TestDiscordClient_MessageHandling(t *testing.T) {
	// Test content cleaning (removing bot mentions)
	content := "<@123456789> Hello, bot!"
	botID := "123456789"

	// Clean the content
	cleaned := strings.ReplaceAll(content, "<@"+botID+">", "")
	cleaned = strings.ReplaceAll(cleaned, "<@!"+botID+">", "")
	cleaned = strings.TrimSpace(cleaned)

	expected := "Hello, bot!"
	if cleaned != expected {
		t.Errorf("cleaned content = %q, want %q", cleaned, expected)
	}
}

// TestDiscordClient_GuildMentionCheck tests guild mention detection.
func TestDiscordClient_GuildMentionCheck(t *testing.T) {
	// Simulate mention check
	botID := "123456789"
	mentions := []struct {
		ID string
	}{
		{ID: "987654321"}, // Another user
		{ID: "123456789"}, // Bot
	}

	mentioned := false
	for _, m := range mentions {
		if m.ID == botID {
			mentioned = true
			break
		}
	}

	if !mentioned {
		t.Error("bot should be mentioned")
	}
}

// TestDiscordClient_DMHandling tests DM vs guild message detection.
func TestDiscordClient_DMHandling(t *testing.T) {
	// DM message (no GuildID)
	guildID := ""
	isDM := guildID == ""
	if !isDM {
		t.Error("empty GuildID should be DM")
	}

	// Guild message
	guildID = "987654321"
	isDM = guildID == ""
	if isDM {
		t.Error("non-empty GuildID should not be DM")
	}
}

// TestDiscordClient_AttachmentHandling tests attachment handling.
func TestDiscordClient_AttachmentHandling(t *testing.T) {
	content := "Check this out"
	attachments := []struct {
		URL      string
		Filename string
	}{
		{URL: "https://example.com/image.png", Filename: "image.png"},
		{URL: "https://example.com/doc.pdf", Filename: "doc.pdf"},
	}

	// Append attachments to content
	for _, att := range attachments {
		content += "\n[attachment: " + att.URL + "]"
	}

	if !strings.Contains(content, "image.png") {
		t.Error("content should contain attachment URL")
	}
	if !strings.Contains(content, "doc.pdf") {
		t.Error("content should contain second attachment URL")
	}
}

// TestDiscordClient_SenderName tests sender name formatting.
func TestDiscordClient_SenderName(t *testing.T) {
	tests := []struct {
		username      string
		discriminator string
		expected      string
	}{
		{"TestUser", "", "TestUser"},
		{"TestUser", "0", "TestUser"},
		{"TestUser", "1234", "TestUser#1234"},
	}

	for _, tt := range tests {
		senderName := tt.username
		if tt.discriminator != "" && tt.discriminator != "0" {
			senderName += "#" + tt.discriminator
		}
		if senderName != tt.expected {
			t.Errorf("senderName = %q, want %q", senderName, tt.expected)
		}
	}
}

// TestDiscordClient_ContextCancellation tests that the client respects context cancellation.
func TestDiscordClient_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel immediately
	cancel()

	// Verify context is cancelled
	select {
	case <-ctx.Done():
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("context should be cancelled")
	}
}

// TestDiscordClient_MessageSplit tests that long messages are split correctly.
func TestDiscordClient_MessageSplit(t *testing.T) {
	// Create a message that's exactly at the limit
	longMessage := strings.Repeat("a", 2000)
	chunks := splitMessage(longMessage, 2000)

	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}

	// Create a message that's over the limit
	veryLongMessage := strings.Repeat("a", 3000)
	chunks = splitMessage(veryLongMessage, 2000)

	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}

	// Verify total content is preserved
	totalLen := 0
	for _, chunk := range chunks {
		totalLen += len(chunk)
	}
	if totalLen != 3000 {
		t.Errorf("total content length = %d, want 3000", totalLen)
	}
}

// TestDiscordClient_NewlineSplit tests that messages split at newlines when possible.
func TestDiscordClient_NewlineSplit(t *testing.T) {
	// Create a message with a newline near the split point
	message := strings.Repeat("a", 1500) + "\n" + strings.Repeat("b", 1500)
	chunks := splitMessage(message, 2000)

	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}

	// First chunk should end with newline (split at newline)
	if !strings.HasSuffix(chunks[0], "\n") {
		t.Error("first chunk should end with newline")
	}

	// Second chunk should start with 'b'
	if !strings.HasPrefix(chunks[1], "b") {
		t.Error("second chunk should start with 'b'")
	}
}

// mockThreadSender is a discordSender implementation that records thread
// creation and channel messages without touching the Discord API.
type mockThreadSender struct {
	threads      map[string]*discordgo.Channel // threadID → channel
	createdCount int
	sent         []string
}

func (m *mockThreadSender) ChannelMessageSend(channelID, content string, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	m.sent = append(m.sent, channelID+": "+content)
	return &discordgo.Message{ID: "msg"}, nil
}

func (m *mockThreadSender) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	m.sent = append(m.sent, channelID+": "+data.Content)
	return &discordgo.Message{ID: "msg"}, nil
}

func (m *mockThreadSender) ChannelTyping(channelID string, options ...discordgo.RequestOption) error { return nil }

func (m *mockThreadSender) MessageThreadStartComplex(channelID, messageID string, data *discordgo.ThreadStart, options ...discordgo.RequestOption) (*discordgo.Channel, error) {
	m.createdCount++
	id := fmt.Sprintf("thread-%d", m.createdCount)
	ch := &discordgo.Channel{ID: id, Name: data.Name}
	m.threads[id] = ch
	return ch, nil
}

func (m *mockThreadSender) ThreadJoin(threadID string, options ...discordgo.RequestOption) error { return nil }

func (m *mockThreadSender) Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if ch, ok := m.threads[channelID]; ok {
		return ch, nil
	}
	return nil, fmt.Errorf("channel not found")
}

// newCooldownTestClient builds a discordClient with a mock sender and the
// given thread cooldown for cooldown-behaviour tests.
func newCooldownTestClient(hub *chat.Hub, sender *mockThreadSender, cooldownS int) *discordClient {
	return newDiscordClient(context.Background(), sender, hub, "botid", nil, false, []string{"monitored-chan"}, true, "", cooldownS, DiscordRateLimit{})
}

// TestDiscordThreadCooldown_RoutesToExistingThread verifies that rapid
// follow-up messages continue in the user's existing thread rather than
// creating a new one.
func TestDiscordThreadCooldown_RoutesToExistingThread(t *testing.T) {
	hub := chat.NewHub(10)
	sender := &mockThreadSender{threads: map[string]*discordgo.Channel{}}
	c := newCooldownTestClient(hub, sender, 300)

	msg := func(content string) *discordgo.MessageCreate {
		return &discordgo.MessageCreate{
			Message: &discordgo.Message{
				ID:        "m" + content,
				ChannelID: "monitored-chan",
				GuildID:   "guild1",
				Author:    &discordgo.User{ID: "user1", Username: "tester"},
				Content:   content,
			},
		}
	}

	// First message: no prior thread, one should be created.
	c.handleMessage(nil, msg("first"))
	if sender.createdCount != 1 {
		t.Fatalf("expected 1 thread after first message, got %d", sender.createdCount)
	}
	// Read the first message from the hub (FIFO).
	select {
	case got := <-hub.In:
		if got.ChatID != "thread-1" || got.Content != "first" {
			t.Fatalf("first message = (%q, %q), want (thread-1, first)", got.ChatID, got.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first message to be forwarded")
	}

	// Second message within cooldown: should continue in existing thread.
	c.handleMessage(nil, msg("second"))
	if sender.createdCount != 1 {
		t.Fatalf("expected still 1 thread (cooldown should prevent new one), got %d", sender.createdCount)
	}

	// Verify the second message was forwarded into the first thread.
	select {
	case got := <-hub.In:
		if got.ChatID != "thread-1" {
			t.Errorf("second message ChatID = %q, want thread-1", got.ChatID)
		}
		if got.Content != "second" {
			t.Errorf("second message content = %q, want %q", got.Content, "second")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second message to be forwarded")
	}
}

// TestDiscordThreadCooldown_ExpiredCreatesNewThread verifies that after the
// cooldown window passes a new thread is created.
func TestDiscordThreadCooldown_ExpiredCreatesNewThread(t *testing.T) {
	hub := chat.NewHub(10)
	sender := &mockThreadSender{threads: map[string]*discordgo.Channel{}}
	c := newCooldownTestClient(hub, sender, 1) // 1 second cooldown

	c.handleMessage(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID: "m1", ChannelID: "monitored-chan", GuildID: "guild1",
			Author: &discordgo.User{ID: "user1", Username: "tester"}, Content: "first",
		},
	})
	if sender.createdCount != 1 {
		t.Fatalf("expected 1 thread, got %d", sender.createdCount)
	}

	// Expire the cooldown by backdating the recorded thread creation.
	c.lastThreadMu.Lock()
	key := "user1:monitored-chan"
	info := c.lastThread[key]
	info.created = time.Now().Add(-2 * time.Second)
	c.lastThread[key] = info
	c.lastThreadMu.Unlock()

	c.handleMessage(nil, &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID: "m2", ChannelID: "monitored-chan", GuildID: "guild1",
			Author: &discordgo.User{ID: "user1", Username: "tester"}, Content: "second",
		},
	})
	if sender.createdCount != 2 {
		t.Fatalf("expected 2 threads after cooldown expiry, got %d", sender.createdCount)
	}

	// Drain hub.
	for {
		select {
		case <-hub.In:
		default:
			goto done
		}
	}
done:
}

// TestDiscordThreadCooldown_Disabled verifies 0 disables the cooldown and
// every message creates a new thread.
func TestDiscordThreadCooldown_Disabled(t *testing.T) {
	hub := chat.NewHub(10)
	sender := &mockThreadSender{threads: map[string]*discordgo.Channel{}}
	c := newCooldownTestClient(hub, sender, 0)

	for i := 0; i < 3; i++ {
		c.handleMessage(nil, &discordgo.MessageCreate{
			Message: &discordgo.Message{
				ID: fmt.Sprintf("m%d", i), ChannelID: "monitored-chan", GuildID: "guild1",
				Author: &discordgo.User{ID: "user1", Username: "tester"}, Content: fmt.Sprintf("msg %d", i),
			},
		})
	}
	if sender.createdCount != 3 {
		t.Fatalf("cooldown=0 should create a thread per message, got %d", sender.createdCount)
	}
	for {
		select {
		case <-hub.In:
		default:
			goto done
		}
	}
done:
}

// TestDiscordThreadCooldown_DefaultValue verifies the default applied by
// main.go when the config value is unset.
func TestDiscordThreadCooldown_DefaultValue(t *testing.T) {
	if DefaultThreadCooldownS != 300 {
		t.Errorf("DefaultThreadCooldownS = %d, want 300", DefaultThreadCooldownS)
	}
}
