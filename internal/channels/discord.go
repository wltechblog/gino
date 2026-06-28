//go:build !only_telegram && !only_slack && !only_whatsapp

package channels

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/wltechblog/gino/internal/chat"
)

// discordSender is the subset of *discordgo.Session used for outbound operations.
// It exists to enable testing without a live Discord WebSocket connection.
type discordSender interface {
	ChannelMessageSend(channelID, content string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelTyping(channelID string, options ...discordgo.RequestOption) error
	MessageThreadStartComplex(channelID, messageID string, data *discordgo.ThreadStart, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	Channel(channelID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
}

// StartDiscord starts a Discord bot using the discordgo library.
// allowFrom restricts which Discord user IDs may send messages; empty means allow all.
// DiscordRateLimit holds rate-limiting configuration for Discord.
type DiscordRateLimit struct {
	PerMinute int // max messages per user per minute (0 = unlimited)
	PerHour   int // max messages per user per hour (0 = unlimited)
	TotalHour int // max total messages per hour across all users (0 = unlimited)
}

func StartDiscord(ctx context.Context, hub *chat.Hub, token string, allowFrom []string, allowDMs bool, monitorChannels []string, rl DiscordRateLimit) error {
	if token == "" {
		return fmt.Errorf("discord token not provided")
	}

	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return fmt.Errorf("failed to create discord session: %w", err)
	}

	// Enable state so we can look up channel types (thread detection).
	session.StateEnabled = true

	session.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent

	if err := session.Open(); err != nil {
		return fmt.Errorf("failed to open discord connection: %w", err)
	}

	botUser, err := session.User("@me")
	if err != nil {
		if closeErr := session.Close(); closeErr != nil {
			log.Printf("discord: error closing session: %v", closeErr)
		}
		return fmt.Errorf("failed to get bot user: %w", err)
	}
	log.Printf("discord: connected as %s (%s)", botUser.Username, botUser.ID)

	client := newDiscordClient(ctx, session, hub, botUser.ID, allowFrom, allowDMs, monitorChannels, rl)
	log.Printf("discord: monitored channels: %v", monitorChannels)
	session.AddHandler(client.handleMessage)
	go client.runOutbound()
	go func() {
		<-ctx.Done()
		log.Println("discord: shutting down")
		client.stopAllTyping()
		if err := session.Close(); err != nil {
			log.Printf("discord: error closing session: %v", err)
		}
	}()

	return nil
}

// discordClient handles Discord messaging using a discordSender.
type discordClient struct {
	sender           discordSender
	hub              *chat.Hub
	outCh            <-chan chat.Outbound
	botID            string
	allowed          map[string]struct{}
	allowDMs         bool
	monitorChannels  map[string]struct{} // channel IDs where bot engages without mention
	ctx              context.Context
	typingMu         sync.Mutex
	typingStop       map[string]chan struct{}
	threadOwner      map[string]string // threadID → owner userID
	ownerMu          sync.RWMutex
	rateLimit        DiscordRateLimit
	rateMu           sync.Mutex
	userMinute       map[string][]time.Time // userID → timestamps of messages in current minute window
	userHour         map[string][]time.Time // userID → timestamps of messages in current hour window
	totalHour        []time.Time            // timestamps of all messages in current hour window
}

// newDiscordClient constructs a discordClient and registers it as the hub's
// "discord" outbound subscriber. Inject a mock discordSender for tests.
func newDiscordClient(ctx context.Context, sender discordSender, hub *chat.Hub, botID string, allowFrom []string, allowDMs bool, monitorChannels []string, rl DiscordRateLimit) *discordClient {
	allowed := make(map[string]struct{}, len(allowFrom))
	for _, id := range allowFrom {
		allowed[id] = struct{}{}
	}
	monitor := make(map[string]struct{}, len(monitorChannels))
	for _, id := range monitorChannels {
		monitor[id] = struct{}{}
	}
	return &discordClient{
		sender:          sender,
		hub:             hub,
		outCh:           hub.Subscribe("discord"),
		botID:           botID,
		allowed:         allowed,
		allowDMs:        allowDMs,
		monitorChannels: monitor,
		ctx:             ctx,
		typingStop:      make(map[string]chan struct{}),
		threadOwner:     make(map[string]string),
		rateLimit:       rl,
		userMinute:      make(map[string][]time.Time),
		userHour:        make(map[string][]time.Time),
	}
}

// isMonitored returns true if the channel is in the monitor list (bot engages
// without requiring an @mention).
func (c *discordClient) isMonitored(channelID string) bool {
	_, ok := c.monitorChannels[channelID]
	return ok
}

// isThread checks whether a channel is a Discord thread (public, private, or news thread).
func (c *discordClient) isThread(channelID string) bool {
	ch, err := c.sender.Channel(channelID)
	if err != nil {
		return false
	}
	return ch.IsThread()
}

// parentChannelID returns the parent channel ID for a thread, or the channelID itself if not a thread.
func (c *discordClient) parentChannelID(channelID string) string {
	ch, err := c.sender.Channel(channelID)
	if err != nil {
		return channelID
	}
	if ch.ParentID != "" {
		return ch.ParentID
	}
	return channelID
}

// checkRateLimit returns true if the user is allowed to send a message.
// It tracks per-user and global message counts using sliding time windows.
func (c *discordClient) checkRateLimit(userID string) bool {
	rl := c.rateLimit
	if rl.PerMinute == 0 && rl.PerHour == 0 && rl.TotalHour == 0 {
		return true // no limits configured
	}

	c.rateMu.Lock()
	defer c.rateMu.Unlock()

	now := time.Now()
	minuteAgo := now.Add(-time.Minute)
	hourAgo := now.Add(-time.Hour)

	// Check per-user per-minute limit.
	if rl.PerMinute > 0 {
		c.userMinute[userID] = pruneOld(c.userMinute[userID], minuteAgo)
		if len(c.userMinute[userID]) >= rl.PerMinute {
			return false
		}
	}

	// Check per-user per-hour limit.
	if rl.PerHour > 0 {
		c.userHour[userID] = pruneOld(c.userHour[userID], hourAgo)
		if len(c.userHour[userID]) >= rl.PerHour {
			return false
		}
	}

	// Check total per-hour limit.
	if rl.TotalHour > 0 {
		c.totalHour = pruneOld(c.totalHour, hourAgo)
		if len(c.totalHour) >= rl.TotalHour {
			return false
		}
	}

	// All checks passed — record the message.
	if rl.PerMinute > 0 {
		c.userMinute[userID] = append(c.userMinute[userID], now)
	}
	if rl.PerHour > 0 {
		c.userHour[userID] = append(c.userHour[userID], now)
	}
	if rl.TotalHour > 0 {
		c.totalHour = append(c.totalHour, now)
	}

	// Periodically prune users with no recent activity to prevent unbounded growth.
	if len(c.userMinute) > 1000 {
		c.pruneStaleUsers(now)
	}

	return true
}

// pruneStaleUsers removes users with no recent activity from the rate limiter maps.
func (c *discordClient) pruneStaleUsers(now time.Time) {
	hourAgo := now.Add(-time.Hour)
	for userID, timestamps := range c.userMinute {
		pruned := pruneOld(timestamps, now.Add(-time.Minute))
		if len(pruned) == 0 {
			delete(c.userMinute, userID)
		} else {
			c.userMinute[userID] = pruned
		}
	}
	for userID, timestamps := range c.userHour {
		pruned := pruneOld(timestamps, hourAgo)
		if len(pruned) == 0 {
			delete(c.userHour, userID)
		} else {
			c.userHour[userID] = pruned
		}
	}
}

// pruneOld removes timestamps older than cutoff from the slice.
func pruneOld(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for ; i < len(ts); i++ {
		if !ts[i].Before(cutoff) {
			break
		}
	}
	if i > 0 {
		return ts[i:]
	}
	return ts
}

// handleMessage is the discordgo MessageCreate event handler.
// The *discordgo.Session parameter is intentionally ignored; all bot-identity
// information is held in c.botID so that we can call this in tests without a
// live session.
func (c *discordClient) handleMessage(_ *discordgo.Session, m *discordgo.MessageCreate) {
	log.Printf("discord: handleMessage: author=%s bot=%v channel=%s guild=%s content=%q", m.Author.Username, m.Author.Bot, m.ChannelID, m.GuildID, truncate(m.Content, 80))

	if m.Author == nil || m.Author.Bot || m.Author.ID == c.botID {
		return
	}

	// Enforce allowlist when one is configured.
	if len(c.allowed) > 0 {
		if _, ok := c.allowed[m.Author.ID]; !ok {
			log.Printf("discord: dropped message from unauthorised user %s (%s)", m.Author.Username, m.Author.ID)
			return
		}
	}

	isDM := m.GuildID == ""

	// DM handling: only allowed if allowDMs is true.
	if isDM {
		if !c.allowDMs {
			return
		}
		// Rate limit DMs.
		if !c.checkRateLimit(m.Author.ID) {
			log.Printf("discord: rate limited user %s (%s) in DM", m.Author.Username, m.Author.ID)
			return
		}
		// DMs go through directly as a conversation keyed on the DM channel.
		c.forwardMessage(m, m.ChannelID, true)
		return
	}

	// Guild channel handling.

	// Monitored channels: engage on every message without requiring a mention.
	// Reply directly in-channel (no thread creation). Session is keyed on the channel.
	if c.isMonitored(m.ChannelID) {
		log.Printf("discord: channel %s is monitored, forwarding message", m.ChannelID)
		if !c.checkRateLimit(m.Author.ID) {
			log.Printf("discord: rate limited user %s (%s) in monitored channel %s", m.Author.Username, m.Author.ID, m.ChannelID)
			if _, err := c.sender.ChannelMessageSend(m.ChannelID, fmt.Sprintf("⏳ <@%s> You're being rate limited. Please wait a moment before sending more messages.", m.Author.ID)); err != nil {
				log.Printf("discord: failed to send rate limit notice: %v", err)
			}
			return
		}
		c.forwardMessage(m, m.ChannelID, false)
		return
	}

	// If the message is already inside a thread, treat it as a continuation
	// of that conversation. The thread owner can send freely; other users
	// must @ the bot to join the conversation.
	if c.isThread(m.ChannelID) {
		c.ownerMu.RLock()
		ownerID, hasOwner := c.threadOwner[m.ChannelID]
		c.ownerMu.RUnlock()

		if hasOwner && ownerID != m.Author.ID {
			// Non-owner inside someone else's thread.
			// Only respond if they @mention the bot, then create a new thread for them.
			mentioned := false
			for _, u := range m.Mentions {
				if u.ID == c.botID {
					mentioned = true
					break
				}
			}
			if !mentioned {
				return
			}
			// Rate limit non-owner.
			if !c.checkRateLimit(m.Author.ID) {
				log.Printf("discord: rate limited user %s (%s) in thread %s", m.Author.Username, m.Author.ID, m.ChannelID)
				if _, err := c.sender.ChannelMessageSend(m.ChannelID, fmt.Sprintf("⏳ <@%s> You're being rate limited. Please wait a moment before sending more messages.", m.Author.ID)); err != nil {
					log.Printf("discord: failed to send rate limit notice: %v", err)
				}
				return
			}
			parentID := c.parentChannelID(m.ChannelID)
			c.createThreadAndForward(m, parentID)
			return
		}

		// Thread owner: forward message as continuation.
		// Rate limit thread owner.
		if !c.checkRateLimit(m.Author.ID) {
			log.Printf("discord: rate limited user %s (%s) in thread %s", m.Author.Username, m.Author.ID, m.ChannelID)
			if _, err := c.sender.ChannelMessageSend(m.ChannelID, fmt.Sprintf("⏳ <@%s> You're being rate limited. Please wait a moment before sending more messages.", m.Author.ID)); err != nil {
				log.Printf("discord: failed to send rate limit notice: %v", err)
			}
			return
		}
		c.forwardMessage(m, m.ChannelID, false)
		return
	}

	// In a regular guild channel, the bot only responds when @-mentioned.
	mentioned := false
	for _, u := range m.Mentions {
		if u.ID == c.botID {
			mentioned = true
			break
		}
	}
	if !mentioned {
		return
	}

	// Rate limit guild channel @mention.
	if !c.checkRateLimit(m.Author.ID) {
		log.Printf("discord: rate limited user %s (%s) in channel %s", m.Author.Username, m.Author.ID, m.ChannelID)
		if _, err := c.sender.ChannelMessageSend(m.ChannelID, fmt.Sprintf("⏳ <@%s> You're being rate limited. Please wait a moment before sending more messages.", m.Author.ID)); err != nil {
			log.Printf("discord: failed to send rate limit notice: %v", err)
		}
		return
	}

	// Create a thread from the user's message and reply in it.
	c.createThreadAndForward(m, m.ChannelID)
}

// forwardMessage strips mentions, builds the inbound message, and sends it to the hub.
// createThreadAndForward creates a new Discord thread from the user's message
// in the given parent channel, records ownership, and forwards the message.
func (c *discordClient) createThreadAndForward(m *discordgo.MessageCreate, parentChannelID string) {
	threadName := fmt.Sprintf("%s — %s", senderDisplayName(m.Author), truncate(m.Content, 40))
	thread, err := c.sender.MessageThreadStartComplex(parentChannelID, m.Message.ID, &discordgo.ThreadStart{
		Name:                threadName,
		AutoArchiveDuration: 10080, // 1 week (max)
		Type:                discordgo.ChannelTypeGuildPublicThread,
	})
	if err != nil {
		log.Printf("discord: failed to create thread: %v", err)
		// Don't fall back to parent channel — just drop it so we never reply outside a thread.
		return
	}

	log.Printf("discord: created thread %s (%s) for message from %s", thread.ID, thread.Name, senderDisplayName(m.Author))

	// Record the thread owner so we can enforce ownership.
	c.ownerMu.Lock()
	c.threadOwner[thread.ID] = m.Author.ID
	c.ownerMu.Unlock()

	// Forward the message into the hub using the thread ID as the chat ID.
	// This creates a new session keyed on discord:<threadID>.
	c.forwardMessage(m, thread.ID, false)
}

func (c *discordClient) forwardMessage(m *discordgo.MessageCreate, chatID string, isDM bool) {
	// Strip bot @-mentions from the message text.
	content := m.Content
	for _, u := range m.Mentions {
		if u.ID == c.botID {
			content = strings.ReplaceAll(content, "<@"+u.ID+">", "")
			content = strings.ReplaceAll(content, "<@!"+u.ID+">", "")
		}
	}
	content = strings.TrimSpace(content)

	// Append file attachment URLs as inline references.
	for _, att := range m.Attachments {
		content += fmt.Sprintf("\n[attachment: %s]", att.URL)
	}

	if content == "" {
		return
	}

	senderName := senderDisplayName(m.Author)
	log.Printf("discord: message from %s (%s) in %s: %s", senderName, m.Author.ID, chatID, truncate(content, 50))

	c.startTyping(chatID)

	c.hub.In <- chat.Inbound{
		Channel:   "discord",
		SenderID:  m.Author.ID,
		ChatID:    chatID,
		Content:   content,
		Timestamp: time.Now(),
		Metadata: map[string]interface{}{
			"username":   senderName,
			"guild_id":   m.GuildID,
			"channel_id": m.ChannelID,
			"is_dm":      isDM,
		},
	}
}

// runOutbound reads replies from the hub's discord subscription and sends them.
func (c *discordClient) runOutbound() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case out := <-c.outCh:
			c.stopTyping(out.ChatID)
			for _, chunk := range splitMessage(out.Content, 2000) {
				if _, err := c.sender.ChannelMessageSend(out.ChatID, chunk); err != nil {
					log.Printf("discord: send error: %v", err)
				}
			}
		}
	}
}

// startTyping begins (or resets) a continuous typing indicator for a channel.
// It stops automatically after 5 minutes or when stopTyping / stopAllTyping is called.
func (c *discordClient) startTyping(channelID string) {
	c.typingMu.Lock()
	if stop, ok := c.typingStop[channelID]; ok {
		close(stop)
	}
	stop := make(chan struct{})
	c.typingStop[channelID] = stop
	c.typingMu.Unlock()

	go func() {
		if err := c.sender.ChannelTyping(channelID); err != nil {
			log.Printf("discord: typing error: %v", err)
		}

		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		timeout := time.NewTimer(5 * time.Minute)
		defer timeout.Stop()

		for {
			select {
			case <-stop:
				return
			case <-timeout.C:
				return
			case <-c.ctx.Done():
				return
			case <-ticker.C:
				if err := c.sender.ChannelTyping(channelID); err != nil {
					log.Printf("discord: typing error: %v", err)
				}
			}
		}
	}()
}

// stopTyping cancels the typing indicator for the given channel.
func (c *discordClient) stopTyping(channelID string) {
	c.typingMu.Lock()
	defer c.typingMu.Unlock()
	if stop, ok := c.typingStop[channelID]; ok {
		close(stop)
		delete(c.typingStop, channelID)
	}
}

// stopAllTyping cancels all active typing indicators.
func (c *discordClient) stopAllTyping() {
	c.typingMu.Lock()
	defer c.typingMu.Unlock()
	for _, stop := range c.typingStop {
		close(stop)
	}
	c.typingStop = make(map[string]chan struct{})
}

// senderDisplayName returns "Username" for new-style accounts or
// "Username#Discriminator" for legacy accounts.
func senderDisplayName(u *discordgo.User) string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	if u.Discriminator != "" && u.Discriminator != "0" {
		return u.Username + "#" + u.Discriminator
	}
	return u.Username
}
