//go:build !only_telegram && !only_slack && !only_whatsapp

package channels

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelTyping(channelID string, options ...discordgo.RequestOption) error
	MessageThreadStartComplex(channelID, messageID string, data *discordgo.ThreadStart, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ThreadJoin(threadID string, options ...discordgo.RequestOption) error
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

// DefaultThreadCooldownS is the per-user thread creation cooldown (seconds)
// applied when discord.threadCooldownS is unset in config.
const DefaultThreadCooldownS = 300

func StartDiscord(ctx context.Context, hub *chat.Hub, token string, allowFrom []string, allowDMs bool, monitorChannels []string, sendAttachments bool, adminRoleID string, threadCooldownS int, rl DiscordRateLimit) error {
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

	client := newDiscordClient(ctx, session, hub, botUser.ID, allowFrom, allowDMs, monitorChannels, sendAttachments, adminRoleID, threadCooldownS, rl)
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
	sendAttachments  bool                // whether to send file attachments outbound
	adminRoleID      string              // Discord role ID that can post in any thread
	threadCooldown   time.Duration       // per-user cooldown before a new thread may be created
	ctx              context.Context
	typingMu         sync.Mutex
	typingStop       map[string]chan struct{}
	threadOwner      map[string]string // threadID → owner userID
	ownerMu          sync.RWMutex
	lastThread       map[string]lastThreadInfo // "userID:channelID" → last thread info
	lastThreadMu     sync.Mutex
	rateLimit        DiscordRateLimit
	rateMu           sync.Mutex
	userMinute       map[string][]time.Time // userID → timestamps of messages in current minute window
	userHour         map[string][]time.Time // userID → timestamps of messages in current hour window
	totalHour        []time.Time            // timestamps of all messages in current hour window
}

// lastThreadInfo records the most recent thread created for a user in a
// channel, so the cooldown logic can route follow-up messages into it.
type lastThreadInfo struct {
	threadID string
	created  time.Time
}

// newDiscordClient constructs a discordClient and registers it as the hub's
// "discord" outbound subscriber. Inject a mock discordSender for tests.
func newDiscordClient(ctx context.Context, sender discordSender, hub *chat.Hub, botID string, allowFrom []string, allowDMs bool, monitorChannels []string, sendAttachments bool, adminRoleID string, threadCooldownS int, rl DiscordRateLimit) *discordClient {
	cooldown := time.Duration(threadCooldownS) * time.Second
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
		sendAttachments: sendAttachments,
		adminRoleID:     adminRoleID,
		ctx:             ctx,
		typingStop:      make(map[string]chan struct{}),
		threadOwner:     make(map[string]string),
		lastThread:      make(map[string]lastThreadInfo),
		threadCooldown:  cooldown,
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

// recentThread returns the thread a user most recently started in the given
// channel, if it was created within the cooldown window and still exists.
// The second return value reports whether such a thread was found.
func (c *discordClient) recentThread(userID, channelID string) (string, bool) {
	if c.threadCooldown <= 0 {
		return "", false // cooldown disabled
	}
	c.lastThreadMu.Lock()
	info, ok := c.lastThread[userID+":"+channelID]
	c.lastThreadMu.Unlock()
	if !ok || time.Since(info.created) >= c.threadCooldown {
		return "", false
	}
	// Verify the thread still exists (it may have been deleted or the bot
	// restarted). If the lookup fails, treat it as expired.
	if _, err := c.sender.Channel(info.threadID); err != nil {
		return "", false
	}
	return info.threadID, true
}

// recordThreadCreated notes the thread a user just started in a channel so
// subsequent messages within the cooldown window can be routed into it.
func (c *discordClient) recordThreadCreated(userID, channelID, threadID string) {
	c.lastThreadMu.Lock()
	c.lastThread[userID+":"+channelID] = lastThreadInfo{threadID: threadID, created: time.Now()}
	c.lastThreadMu.Unlock()
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

// hasAdminRole checks whether the message author has the configured admin role.
// This is used to allow admins/moderators to participate in threads they don't own.
func (c *discordClient) hasAdminRole(m *discordgo.MessageCreate) bool {
	if c.adminRoleID == "" {
		return false
	}
	for _, roleID := range m.Member.Roles {
		if roleID == c.adminRoleID {
			return true
		}
	}
	return false
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
	// Create a thread for the conversation (same as @mention behaviour).
	if c.isMonitored(m.ChannelID) {
		log.Printf("discord: channel %s is monitored, forwarding message", m.ChannelID)
		if !c.checkRateLimit(m.Author.ID) {
			log.Printf("discord: rate limited user %s (%s) in monitored channel %s", m.Author.Username, m.Author.ID, m.ChannelID)
			if _, err := c.sender.ChannelMessageSend(m.ChannelID, fmt.Sprintf("⏳ <@%s> You're being rate limited. Please wait a moment before sending more messages.", m.Author.ID)); err != nil {
				log.Printf("discord: failed to send rate limit notice: %v", err)
			}
			return
		}
		// Thread cooldown: route into the user's existing thread when it was
		// started recently, instead of spawning a new one per message.
		if threadID, ok := c.recentThread(m.Author.ID, m.ChannelID); ok {
			log.Printf("discord: thread cooldown active for %s in %s, continuing in existing thread %s", m.Author.Username, m.ChannelID, threadID)
			c.forwardMessage(m, threadID, false)
			return
		}
		c.createThreadAndForward(m, m.ChannelID)
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
			// Admin role members can post freely in any thread.
			if c.hasAdminRole(m) {
				// Admin: forward as a continuation of this thread's conversation.
				c.forwardMessage(m, m.ChannelID, false)
				return
			}
			// Other non-owners must @mention the bot to create their own thread.
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
			if threadID, ok := c.recentThread(m.Author.ID, parentID); ok {
				log.Printf("discord: thread cooldown active for %s in %s, continuing in existing thread %s", m.Author.Username, parentID, threadID)
				c.forwardMessage(m, threadID, false)
				return
			}
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
	// Apply the same thread cooldown as monitored channels.
	if threadID, ok := c.recentThread(m.Author.ID, m.ChannelID); ok {
		log.Printf("discord: thread cooldown active for %s in %s, continuing in existing thread %s", m.Author.Username, m.ChannelID, threadID)
		c.forwardMessage(m, threadID, false)
		return
	}
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

	// Explicitly join the thread. Discord should auto-join the creating bot,
	// but some server configurations require an explicit join.
	if err := c.sender.ThreadJoin(thread.ID); err != nil {
		log.Printf("discord: warning: failed to join thread %s: %v", thread.ID, err)
	}

	// Record the thread for cooldown routing: follow-up messages from this
	// user within the cooldown window continue here rather than in a new thread.
	c.recordThreadCreated(m.Author.ID, parentChannelID, thread.ID)

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
			"username":    senderName,
			"sender_name": senderName, // canonical key — used for speaker labeling and unprivileged-user notices
			"guild_id":    m.GuildID,
			"channel_id":  m.ChannelID,
			"is_dm":       isDM,
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

			// If we have files and attachments are enabled, send them.
			if len(out.Media) > 0 && c.sendAttachments {
				c.sendWithFiles(out.ChatID, out.Content, out.Media)
				continue
			}

			for _, chunk := range splitMessage(out.Content, 2000) {
				if _, err := c.sender.ChannelMessageSend(out.ChatID, chunk); err != nil {
					log.Printf("discord: send error: %v", err)
				}
			}
		}
	}
}

// sendWithFiles sends attachments to a Discord channel.
// The text content is sent first (chunked if needed), then each file.
// If there are only files and no meaningful text, the first file carries the content as message text.
func (c *discordClient) sendWithFiles(channelID, content string, files []string) {
	// Send text first if we have any (beyond just whitespace).
	if strings.TrimSpace(content) != "" {
		for _, chunk := range splitMessage(content, 2000) {
			if _, err := c.sender.ChannelMessageSend(channelID, chunk); err != nil {
				log.Printf("discord: text send error before files: %v", err)
			}
		}
	}

	// Send each file. Discord allows up to 10 attachments per message.
	for _, filePath := range files {
		file, err := os.Open(filePath)
		if err != nil {
			log.Printf("discord: failed to open file %s: %v", filePath, err)
			continue
		}

		name := filepath.Base(filePath)
		msg := &discordgo.MessageSend{
			Files: []*discordgo.File{
				{
					Name:   name,
					Reader: file,
				},
			},
		}

		_, err = c.sender.ChannelMessageSendComplex(channelID, msg)
		file.Close()
		if err != nil {
			log.Printf("discord: file send error (%s): %v", name, err)
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
