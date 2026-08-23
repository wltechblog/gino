//go:build !only_discord && !only_slack && !only_whatsapp

package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wltechblog/gino/internal/chat"
	"sync"
)

const (
	tgMaxRetries     = 3
	tgRetryBaseDelay = 2 * time.Second
	tgMaxMessageLen  = 4096 // Telegram sendMessage limit
	tgMaxCaptionLen  = 1024 // Telegram sendDocument caption limit
)

// redactToken removes the bot token from a Telegram API URL for safe logging.
// e.g. "https://api.telegram.org/bot123:ABC/sendMessage" → "https://api.telegram.org/bot***/sendMessage"
func redactToken(s string) string {
	const prefix = "https://api.telegram.org/bot"
	if strings.HasPrefix(s, prefix) {
		rest := s[len(prefix):]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			return prefix + "***" + rest[slash:]
		}
	}
	return s
}

// retryPostForm retries PostForm calls with exponential backoff.
func retryPostForm(client *http.Client, apiURL string, data url.Values) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < tgMaxRetries; attempt++ {
		if attempt > 0 {
			delay := tgRetryBaseDelay * time.Duration(1<<(attempt-1))
			log.Printf("telegram: retry %d/%d after %v for %s", attempt, tgMaxRetries, delay, redactToken(apiURL))
			time.Sleep(delay)
		}
		resp, err := client.PostForm(apiURL, data)
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("telegram: %d retries exhausted: %w", tgMaxRetries, lastErr)
}

// retryPost retries Post calls with exponential backoff.
func retryPost(client *http.Client, apiURL, contentType string, body *bytes.Buffer) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < tgMaxRetries; attempt++ {
		if attempt > 0 {
			delay := tgRetryBaseDelay * time.Duration(1<<(attempt-1))
			log.Printf("telegram: retry %d/%d after %v for %s", attempt, tgMaxRetries, delay, redactToken(apiURL))
			time.Sleep(delay)
		}
		resp, err := client.Post(apiURL, contentType, bytes.NewReader(body.Bytes()))
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("telegram: %d retries exhausted: %w", tgMaxRetries, lastErr)
}

func StartTelegram(ctx context.Context, hub *chat.Hub, token string, allowFrom []string, showTyping bool, workspace string, monitorGroups []string) error {
	if token == "" {
		return fmt.Errorf("telegram token not provided")
	}
	base := "https://api.telegram.org/bot" + token
	return StartTelegramWithBase(ctx, hub, token, base, allowFrom, showTyping, workspace, monitorGroups)
}

func StartTelegramWithBase(ctx context.Context, hub *chat.Hub, token, base string, allowFrom []string, showTyping bool, workspace string, monitorGroups []string) error {
	if base == "" {
		return fmt.Errorf("base URL is required")
	}

	allowed := make(map[string]struct{}, len(allowFrom))
	for _, id := range allowFrom {
		allowed[id] = struct{}{}
	}

	// Build monitorGroups lookup set
	monitored := make(map[string]struct{}, len(monitorGroups))
	for _, id := range monitorGroups {
		monitored[strings.TrimSpace(id)] = struct{}{}
	}

	client := &http.Client{Timeout: 45 * time.Second}
	fileBase := strings.Replace(base, "/bot"+token, "/file/bot"+token, 1)

	// Get bot username and ID for @mention and reply detection
	botUsername := ""
	botUserID := int64(0)
	if resp, err := client.PostForm(base+"/getMe", url.Values{}); err == nil {
		var me struct {
			Ok     bool `json:"ok"`
			Result struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"result"`
		}
		if decodeErr := json.NewDecoder(resp.Body).Decode(&me); err == nil && decodeErr == nil && me.Ok {
			botUsername = strings.ToLower(me.Result.Username)
			botUserID = me.Result.ID
		}
		resp.Body.Close()
	}
	if botUsername != "" {
		log.Printf("telegram: bot @%s (ID %d), monitoring %d group(s)", botUsername, botUserID, len(monitorGroups))
	}

	typingMu := new(sync.Mutex)
	typingChats := make(map[string]struct{})
	typingDone := make(map[string]chan struct{})

	startTyping := func(chatID string) {
		typingMu.Lock()
		if _, exists := typingChats[chatID]; exists {
			typingMu.Unlock()
			return
		}
		typingChats[chatID] = struct{}{}
		done := make(chan struct{})
		typingDone[chatID] = done
		typingMu.Unlock()
		go func() {
			defer func() {
				typingMu.Lock()
				delete(typingChats, chatID)
				delete(typingDone, chatID)
				typingMu.Unlock()
			}()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				v := url.Values{}
				v.Set("chat_id", chatID)
				v.Set("action", "typing")
				resp, err := retryPostForm(client, base+"/sendChatAction", v)
				if err != nil {
					log.Printf("telegram sendChatAction error: %v", err)
				} else {
					io.ReadAll(resp.Body)
					resp.Body.Close()
				}
				select {
				case <-done:
					return
				case <-ticker.C:
				}
			}
		}()
	}

	stopTyping := func(chatID string) {
		typingMu.Lock()
		if done, ok := typingDone[chatID]; ok {
			close(done)
		}
		typingMu.Unlock()
	}

	go func() {
		offset := int64(0)
		for {
			select {
			case <-ctx.Done():
				log.Println("telegram: stopping inbound polling")
				return
			default:
			}

			values := url.Values{}
			values.Set("offset", strconv.FormatInt(offset, 10))
			values.Set("timeout", "30")
			resp, err := client.PostForm(base+"/getUpdates", values)
			if err != nil {
				log.Printf("telegram getUpdates error: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				log.Printf("telegram getUpdates: HTTP %d — backing off", resp.StatusCode)
				if resp.StatusCode == 401 {
					log.Printf("telegram getUpdates: 401 Unauthorized — token may be invalid")
				}
				backoff := 5 * time.Second
				if resp.StatusCode == 429 {
					backoff = 30 * time.Second
				}
				time.Sleep(backoff)
				continue
			}
			var gu struct {
				Ok     bool `json:"ok"`
				Result []struct {
					UpdateID int64 `json:"update_id"`
					Message  *struct {
						MessageID int64 `json:"message_id"`
						From      *struct {
							ID        int64  `json:"id"`
							FirstName string `json:"first_name"`
						} `json:"from"`
						Chat struct {
							ID int64 `json:"id"`
						} `json:"chat"`
						Text            string `json:"text"`
						Caption         string `json:"caption"`
						MessageThreadID *int64 `json:"message_thread_id"`
						Document *struct {
							FileID   string `json:"file_id"`
							FileName string `json:"file_name"`
						} `json:"document"`
						Photo []struct {
							FileID   string `json:"file_id"`
							Width    int    `json:"width"`
							Height   int    `json:"height"`
							FileSize int    `json:"file_size"`
						} `json:"photo"`
						ReplyToMessage *struct {
							From *struct {
								ID int64 `json:"id"`
							} `json:"from"`
						} `json:"reply_to_message"`
					} `json:"message"`
					CallbackQuery *struct {
						ID      string `json:"id"`
						Data    string `json:"data"`
						From    struct {
							ID        int64  `json:"id"`
							FirstName string `json:"first_name"`
						} `json:"from"`
						Message *struct {
							MessageID int64 `json:"message_id"`
							Chat      struct {
								ID int64 `json:"id"`
							} `json:"chat"`
							MessageThreadID *int64 `json:"message_thread_id"`
						} `json:"message"`
					} `json:"callback_query"`
				} `json:"result"`
			}
			if err := json.Unmarshal(body, &gu); err != nil {
				log.Printf("telegram: invalid getUpdates response: %v", err)
				continue
			}
			for _, upd := range gu.Result {
				if upd.UpdateID >= offset {
					offset = upd.UpdateID + 1
				}

				// Handle callback queries (inline keyboard button presses)
				if upd.CallbackQuery != nil {
					cq := upd.CallbackQuery
					fromID := strconv.FormatInt(cq.From.ID, 10)
					chatID := ""
					threadID := ""
					if cq.Message != nil {
						chatID = strconv.FormatInt(cq.Message.Chat.ID, 10)
						if cq.Message.MessageThreadID != nil {
							threadID = strconv.FormatInt(*cq.Message.MessageThreadID, 10)
						}
					}

					// Answer the callback first (removes the loading spinner)
					ansVals := url.Values{}
					ansVals.Set("callback_query_id", cq.ID)
					if resp, err := client.PostForm(base+"/answerCallbackQuery", ansVals); err == nil {
						io.ReadAll(resp.Body)
						resp.Body.Close()
					}

					if cq.Data != "" && chatID != "" {
						// PRIVACY: callbacks inherit the sender's privilege, not a blanket true.
						// A group member tapping an inline keyboard button must not
						// open a privileged session. Same logic as regular messages:
						// privileged only for owner DMs (allowlist) or when no
						// allowlist is configured.
						cbPriv := len(allowed) == 0
						if !cbPriv {
							_, cbPriv = allowed[fromID]
						}
						meta := map[string]interface{}{
							"privileged":   cbPriv,
							"session_key":  "telegram:" + chatID,
							"group":        false,
							"sender_name":  cq.From.FirstName,
							"callback_data": cq.Data,
						}
						if threadID != "" {
							meta["thread_id"] = threadID
						}
						hub.In <- chat.Inbound{
							Channel:   "telegram",
							SenderID:  fromID,
							ChatID:    chatID,
							Content:   cq.Data,
							Timestamp: time.Now(),
							Metadata:  meta,
						}
					}
					continue
				}

				if upd.Message == nil {
					continue
				}
				m := upd.Message
				fromID := ""
				if m.From != nil {
					fromID = strconv.FormatInt(m.From.ID, 10)
				}
				chatID := strconv.FormatInt(m.Chat.ID, 10)

				// Determine message context: DM (private) vs group/supergroup
				isGroup := m.Chat.ID < 0
				_, isMonitored := monitored[chatID]
				isAllowedDM := len(allowed) == 0
				if !isAllowedDM {
					_, isAllowedDM = allowed[fromID]
				}

				if isGroup {
					// Group message handling
					if !isMonitored {
						continue // not a monitored group, ignore
					}

					// Require @mention OR reply to the bot's own message
					textForCheck := strings.ToLower(m.Text)
					mentioned := false
					if botUsername != "" {
						mentioned = strings.Contains(textForCheck, "@"+botUsername)
					}
					// Also trigger if this is a reply to the bot's message
					replyToBot := false
					if m.ReplyToMessage != nil && m.ReplyToMessage.From != nil && botUserID != 0 {
						replyToBot = m.ReplyToMessage.From.ID == botUserID
					}

					if !mentioned && !replyToBot {
						continue // no @mention and not a reply to bot, skip
					}

					// Strip the @mention from the content
					content := m.Text
					if botUsername != "" {
						// Remove @botname (case-insensitive)
						lowerContent := strings.ToLower(content)
						for _, prefix := range []string{"@" + botUsername + " ", "@" + botUsername} {
							if strings.HasPrefix(lowerContent, prefix) {
								content = content[len(prefix):]
								break
							}
						}
						// Also remove inline mentions
						content = strings.TrimSpace(strings.ReplaceAll(
							content, "@"+botUsername, ""))
					}
					if m.Text != "" && content == "" {
						content = m.Text // keep original if stripping removed everything
					}
					if content == "" {
						content = m.Caption
					}

					// Get sender display name
					senderName := ""
					if m.From != nil {
						senderName = m.From.FirstName
					}

					log.Printf("telegram: group message from %s (%s) in %s: %s",
						senderName, fromID, chatID, truncate(content, 50))

					// Per-user session in group: telegram:<groupID>:<userID>
					sessionKey := "telegram:" + chatID + ":" + fromID

					meta := map[string]interface{}{
						"privileged":   isAllowedDM,
						"session_key":  sessionKey,
						"group":        true,
						"sender_name":  senderName,
					}
					if m.MessageThreadID != nil {
						meta["thread_id"] = strconv.FormatInt(*m.MessageThreadID, 10)
					}

					hub.In <- chat.Inbound{
						Channel:   "telegram",
						SenderID:  fromID,
						ChatID:    chatID,
						Content:   strings.TrimSpace(content),
						Timestamp: time.Now(),
						Metadata:  meta,
					}
					if showTyping {
						startTyping(chatID)
					}
					continue
				}

				// DM (private chat) — use existing allowFrom gate
				if len(allowed) > 0 {
					if _, ok := allowed[fromID]; !ok {
						log.Printf("telegram: dropping message from unauthorized user %s", fromID)
						continue
					}
				}

				content := m.Text
				if content == "" {
					content = m.Caption
				}
				var media []string

				if m.Document != nil {
					saved, err := tgDownloadFile(client, base, fileBase, m.Document.FileID, m.Document.FileName, chatID, workspace)
					if err != nil {
						log.Printf("telegram: failed to download document: %v", err)
						content += "\n[Failed to download attached file: " + m.Document.FileName + "]"
					} else {
						media = append(media, saved)
						if content == "" {
							content = "[File received: " + m.Document.FileName + "]"
						} else {
							content += "\n[File received: " + m.Document.FileName + "]"
						}
					}
				}

				if len(m.Photo) > 0 {
					photo := m.Photo[len(m.Photo)-1]
					filename := "photo_" + strconv.FormatInt(time.Now().UnixMilli(), 10) + ".jpg"
					saved, err := tgDownloadFile(client, base, fileBase, photo.FileID, filename, chatID, workspace)
					if err != nil {
						log.Printf("telegram: failed to download photo: %v", err)
						content += "\n[Failed to download attached photo]"
					} else {
						media = append(media, saved)
						if content == "" {
							content = "[Photo received]"
						}
					}
				}

				if content == "" && len(media) == 0 {
					continue
				}

			// Get sender display name for DMs
			senderName := ""
			if m.From != nil {
				senderName = m.From.FirstName
			}

			hub.In <- chat.Inbound{
				Channel:   "telegram",
				SenderID:  fromID,
				ChatID:    chatID,
				Content:   content,
				Timestamp: time.Now(),
				Media:     media,
				Metadata: map[string]interface{}{
					"privileged":  true,
					"session_key": "telegram:" + chatID,
					"group":       false,
					"sender_name": senderName,
				},
			}
			if showTyping {
				startTyping(chatID)
			}
			}
		}
	}()

	outCh := hub.Subscribe("telegram")

	go func() {
		outClient := &http.Client{Timeout: 60 * time.Second}
		for {
			select {
			case <-ctx.Done():
				log.Println("telegram: stopping outbound sender")
				return
			case out := <-outCh:
				stopTyping(out.ChatID)
				threadID := ""
				dmFallbackID := "" // if non-empty, DM this user if topic is closed
				replyMarkup := ""  // JSON-encoded InlineKeyboardMarkup
				if out.Metadata != nil {
					if v, ok := out.Metadata["thread_id"]; ok {
						if s, ok := v.(string); ok {
							threadID = s
						}
					}
					// Only set DM fallback for group chats (negative chat ID)
					if out.ChatID != "" && out.ChatID[0] == '-' {
						if v, ok := out.Metadata["sender_id"]; ok {
							if s, ok := v.(string); ok {
								dmFallbackID = s
							}
						}
					}
					if v, ok := out.Metadata["reply_markup"]; ok {
						if s, ok := v.(string); ok {
							replyMarkup = s
						}
					}
				}
				log.Printf("telegram: sending message to %s (%d chars)", out.ChatID, len(out.Content))
				if len(out.Media) > 0 {
					for i, p := range out.Media {
						caption := ""
						if i == 0 {
							caption = truncateCaption(out.Content)
						}
						if err := tgSendDocument(outClient, base, out.ChatID, p, caption, threadID, dmFallbackID); err != nil {
							log.Printf("telegram sendDocument error: %v", err)
						}
					}
					continue
				}
				if err := tgSendChunked(outClient, base, out.ChatID, out.Content, threadID, dmFallbackID, replyMarkup); err != nil {
					log.Printf("telegram sendMessage error: %v", err)
					continue
				}
			}
		}
	}()

	return nil
}

func tgDownloadFile(client *http.Client, base, fileBase, fileID, filename, chatID, workspace string) (string, error) {
	filePath, err := tgGetFilePath(client, base, fileID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(workspace, "uploads", chatID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, filename)

	downloadURL := fileBase + "/" + filePath
	resp, err := client.Get(downloadURL)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: status %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", err
	}
	return dest, nil
}

func tgGetFilePath(client *http.Client, base, fileID string) (string, error) {
	v := url.Values{}
	v.Set("file_id", fileID)
	resp, err := client.PostForm(base+"/getFile", v)
	if err != nil {
		return "", fmt.Errorf("getFile: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		OK   bool `json:"ok"`
		File struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("getFile parse: %w", err)
	}
	if !result.OK || result.File.FilePath == "" {
		return "", fmt.Errorf("getFile no path: %s", strings.TrimSpace(string(body)))
	}
	return result.File.FilePath, nil
}

func tgSendDocument(client *http.Client, base, chatID, filePath, caption, threadID, dmFallbackID string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", chatID)
	if threadID != "" {
		_ = w.WriteField("message_thread_id", threadID)
	}
	if caption != "" {
		_ = w.WriteField("caption", tgEscapeReserved(stripLLMEscapes(caption)))
		_ = w.WriteField("parse_mode", "MarkdownV2")
	}
	part, err := w.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		return fmt.Errorf("form file: %w", err)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("multipart close: %w", err)
	}
	resp, err := retryPost(client, base+"/sendDocument", w.FormDataContentType(), &buf)
	if err != nil {
		return fmt.Errorf("sendDocument: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		return nil
	}
	// If the topic/thread is closed, fall back to DM if available, otherwise General topic
	if resp.StatusCode == 400 && bytes.Contains(body, []byte("TOPIC_CLOSED")) && threadID != "" {
		if dmFallbackID != "" {
			log.Printf("telegram: topic %s is closed in chat %s, falling back to DM %s for document", threadID, chatID, dmFallbackID)
			return tgSendDocument(client, base, dmFallbackID, filePath, caption, "", "")
		}
		log.Printf("telegram: topic %s is closed in chat %s, retrying document without thread_id", threadID, chatID)
		return tgSendDocument(client, base, chatID, filePath, caption, "", "")
	}
	if resp.StatusCode == 400 && bytes.Contains(body, []byte("can't parse entities")) {
		// Debug: log the original and escaped text to diagnose escaping issues
		preview := caption
		if len(preview) > 500 {
			preview = preview[:500] + "..."
		}
		log.Printf("telegram: markdown parse error in caption, retrying as plain text\n"+
			"  API response: %s\n  Original caption: %q", string(body), preview)
		return tgSendDocumentPlain(client, base, chatID, filePath, caption, threadID, dmFallbackID)
	}
	return fmt.Errorf("sendDocument: HTTP %d: %s", resp.StatusCode, string(body))
}

func tgSendDocumentPlain(client *http.Client, base, chatID, filePath, caption, threadID, dmFallbackID string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", chatID)
	if threadID != "" {
		_ = w.WriteField("message_thread_id", threadID)
	}
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	part, err := w.CreateFormFile("document", filepath.Base(filePath))
	if err != nil {
		return fmt.Errorf("form file: %w", err)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("multipart close: %w", err)
	}
	resp, err := retryPost(client, base+"/sendDocument", w.FormDataContentType(), &buf)
	if err != nil {
		return fmt.Errorf("sendDocument: %w", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	return nil
}

// truncateCaption trims content to Telegram's caption limit.
func truncateCaption(content string) string {
	if len(content) <= tgMaxCaptionLen {
		return content
	}
	return content[:tgMaxCaptionLen-3] + "…"
}

// tgSendChunked sends a message, splitting it into chunks if it exceeds the Telegram limit.
// Splits on newlines where possible to avoid breaking sentences/mid-word.
func tgSendChunked(client *http.Client, base, chatID, content, threadID, dmFallbackID, replyMarkup string) error {
	if len(content) <= tgMaxMessageLen {
		return tgSendMessage(client, base, chatID, content, threadID, dmFallbackID, replyMarkup)
	}

	chunks := splitMessage(content, tgMaxMessageLen)
	for i, chunk := range chunks {
		// Only attach reply markup to the last chunk
		markup := ""
		if i == len(chunks)-1 {
			markup = replyMarkup
		}
		if err := tgSendMessage(client, base, chatID, chunk, threadID, dmFallbackID, markup); err != nil {
			return fmt.Errorf("chunk %d/%d: %w", i+1, len(chunks), err)
		}
		if i < len(chunks)-1 {
			time.Sleep(300 * time.Millisecond) // small delay between chunks
		}
	}
	log.Printf("telegram: sent %d chunks to %s", len(chunks), chatID)
	return nil
}

// isWordChar returns true if c is an alphanumeric character or other
// character that commonly appears inside words (e.g. in identifiers).
func isWordChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// findCloseMarker scans forward from pos+1 to find the matching close marker.
// Returns the index of the close marker, or -1 if not found.
func findCloseMarker(s string, pos int, marker byte) int {
	for j := pos + 1; j < len(s); j++ {
		if s[j] == marker {
			return j
		}
	}
	return -1
}

// findCloseMarkerStr scans forward from pos+len(marker) to find matching close.
// Returns the index of the start of the close marker, or -1 if not found.
func findCloseMarkerStr(s string, pos int, marker string) int {
	mlen := len(marker)
	for j := pos + mlen; j+mlen <= len(s); j++ {
		if s[j:j+mlen] == marker {
			return j
		}
	}
	return -1
}

// stripLLMEscapes removes backslash-escaping that the LLM may have added
// before our escaper runs. The LLM sometimes pre-escapes MarkdownV2 special
// characters (e.g. \(, \-, \.) despite instructions not to, which causes
// double-escaping.
func stripLLMEscapes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			next := s[i+1]
			switch next {
			case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!', '\\':
				// LLM-escaped special char — keep just the char, skip the backslash
				b.WriteByte(next)
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// tgEscapeText escapes all MarkdownV2 reserved characters in a string.
// Used for content inside formatting spans where every special char must be escaped.
func tgEscapeText(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// tgEscapeReserved escapes MarkdownV2 reserved characters.
// Telegram requires \ before any of _ * [ ] ( ) ~ ` > # + - = | { } . !
// everywhere except inside code/code-block entities.
//
// Formatting spans (bold, italic, links, etc.) are detected and their
// delimiters preserved, but special chars inside the span content are
// escaped as Telegram requires. Code spans are passed verbatim.
func tgEscapeReserved(s string) string {
	var b strings.Builder
	i := 0
	n := len(s)

	for i < n {
		// Code block ```...``` — preserve verbatim (no escaping inside).
		// An unterminated fence is escaped as literal text instead of
		// emitting an unclosed Pre entity, which Telegram rejects with
		// "Can't find end of Pre entity" (the old code also truncated
		// the trailing content when no closer existed).
		if i+2 < n && s[i] == '`' && s[i+1] == '`' && s[i+2] == '`' {
			end := -1
			for j := i + 3; j+2 < n; j++ {
				if s[j] == '`' && s[j+1] == '`' && s[j+2] == '`' {
					end = j
					break
				}
			}
			if end == -1 {
				b.WriteString("\\`\\`\\`")
				i += 3
				continue
			}
			b.WriteString(s[i : end+3])
			i = end + 3
			continue
		}

		// Inline code `...` — preserve verbatim (no escaping inside)
		if s[i] == '`' {
			closeIdx := findCloseMarker(s, i, '`')
			if closeIdx > i {
				b.WriteString(s[i : closeIdx+1])
				i = closeIdx + 1
				continue
			}
			b.WriteString("\\`")
			i++
			continue
		}

		// Bold *...* — escape content inside
		// Standard markdown **bold** is converted to Telegram's single-asterisk
		// bold. An unterminated ** is escaped as literal text; otherwise the
		// first * would be dropped and the second would open a bogus bold span
		// that swallows everything up to the next stray * (corrupting fences).
		if s[i] == '*' && i+1 < n && s[i+1] == '*' {
			closeIdx := findCloseMarkerStr(s, i, "**")
			if closeIdx > i {
				b.WriteByte('*')
				b.WriteString(tgEscapeText(s[i+2 : closeIdx]))
				b.WriteByte('*')
				i = closeIdx + 2
				continue
			}
			b.WriteString("\\*\\*")
			i += 2
			continue
		}
		if s[i] == '*' && (i+1 >= n || s[i+1] != '*') {
			if i > 0 && isWordChar(s[i-1]) && i+1 < n && isWordChar(s[i+1]) {
				b.WriteString("\\*")
				i++
				continue
			}
			closeIdx := findCloseMarker(s, i, '*')
			if closeIdx > i {
				b.WriteByte('*')
				b.WriteString(tgEscapeText(s[i+1 : closeIdx]))
				b.WriteByte('*')
				i = closeIdx + 1
				continue
			}
			b.WriteString("\\*")
			i++
			continue
		}

		// Underline __...__ — escape content inside
		if i+1 < n && s[i] == '_' && s[i+1] == '_' {
			closeIdx := findCloseMarkerStr(s, i, "__")
			if closeIdx > i {
				b.WriteString("__")
				b.WriteString(tgEscapeText(s[i+2 : closeIdx]))
				b.WriteString("__")
				i = closeIdx + 2
				continue
			}
			b.WriteString("\\_\\_")
			i += 2
			continue
		}

		// Italic _..._ — escape content inside, only at word boundaries
		if s[i] == '_' {
			if i > 0 && isWordChar(s[i-1]) {
				b.WriteString("\\_")
				i++
				continue
			}
			if i+1 < n && !isWordChar(s[i+1]) {
				b.WriteString("\\_")
				i++
				continue
			}
			closeIdx := findCloseMarker(s, i, '_')
			if closeIdx > i {
				b.WriteByte('_')
				b.WriteString(tgEscapeText(s[i+1 : closeIdx]))
				b.WriteByte('_')
				i = closeIdx + 1
				continue
			}
			b.WriteString("\\_")
			i++
			continue
		}

		// Strikethrough ~...~ — escape content inside
		if s[i] == '~' {
			if i+1 < n && s[i+1] == '~' {
				closeIdx := findCloseMarkerStr(s, i, "~~")
				if closeIdx > i {
					b.WriteString("~~")
					b.WriteString(tgEscapeText(s[i+2 : closeIdx]))
					b.WriteString("~~")
					i = closeIdx + 2
					continue
				}
				b.WriteString("\\~\\~")
				i += 2
				continue
			}
			closeIdx := findCloseMarker(s, i, '~')
			if closeIdx > i {
				b.WriteByte('~')
				b.WriteString(tgEscapeText(s[i+1 : closeIdx]))
				b.WriteByte('~')
				i = closeIdx + 1
				continue
			}
			b.WriteString("\\~")
			i++
			continue
		}

		// Spoiler ||...|| — escape content inside
		if i+1 < n && s[i] == '|' && s[i+1] == '|' {
			closeIdx := findCloseMarkerStr(s, i, "||")
			if closeIdx > i {
				b.WriteString("||")
				b.WriteString(tgEscapeText(s[i+2 : closeIdx]))
				b.WriteString("||")
				i = closeIdx + 2
				continue
			}
			b.WriteString("\\|\\|")
			i += 2
			continue
		}

		// Link [text](url) — escape text content, keep URL verbatim
		if s[i] == '[' {
			j := i + 1
			depth := 1
			for j < n && depth > 0 {
				if s[j] == '[' {
					depth++
				} else if s[j] == ']' {
					depth--
				}
				j++
			}
			if depth == 0 {
				closeBracket := j - 1
				if closeBracket+1 < n && s[closeBracket+1] == '(' {
					j = closeBracket + 2
					depth = 1
					for j < n && depth > 0 {
						if s[j] == '(' {
							depth++
						} else if s[j] == ')' {
							depth--
						}
						j++
					}
					if depth == 0 {
						b.WriteByte('[')
						b.WriteString(tgEscapeText(s[i+1 : closeBracket]))
						b.WriteString("](")
						b.WriteString(s[closeBracket+2 : j-1]) // URL verbatim
						b.WriteByte(')')
						i = j
						continue
					}
				}
			}
			b.WriteString("\\[")
			i++
			continue
		}

		// Blockquote > at line start — escape content after marker
		if (i == 0 || s[i-1] == '\n') && s[i] == '>' {
			b.WriteByte('>')
			i++
			if i < n && s[i] == ' ' {
				b.WriteByte(' ')
				i++
			}
			continue
		}

		// Escape reserved character
		switch s[i] {
		case '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!', '\\':
			b.WriteByte('\\')
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
		i++
	}

	return b.String()
}

// tgSendMessage sends a message with MarkdownV2 formatting.
// Reserved characters are escaped to satisfy Telegram's strict parser
// while preserving intentional markdown formatting spans.
// Falls back to plain text on unhandled parse errors.
func tgSendMessage(client *http.Client, base, chatID, text, threadID, dmFallbackID, replyMarkup string) error {
	u := base + "/sendMessage"
	escaped := tgEscapeReserved(stripLLMEscapes(text))
	v := url.Values{}
	v.Set("chat_id", chatID)
	if threadID != "" {
		v.Set("message_thread_id", threadID)
	}
	v.Set("text", escaped)
	v.Set("parse_mode", "MarkdownV2")
	if replyMarkup != "" {
		v.Set("reply_markup", replyMarkup)
	}
	resp, err := retryPostForm(client, u, v)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		return nil
	}
	// If the topic/thread is closed, fall back to DM if available, otherwise General topic
	if resp.StatusCode == 400 && bytes.Contains(body, []byte("TOPIC_CLOSED")) && threadID != "" {
		if dmFallbackID != "" {
			log.Printf("telegram: topic %s is closed in chat %s, falling back to DM %s", threadID, chatID, dmFallbackID)
			return tgSendMessage(client, base, dmFallbackID, text, "", "", replyMarkup)
		}
		log.Printf("telegram: topic %s is closed in chat %s, retrying without thread_id", threadID, chatID)
		return tgSendMessage(client, base, chatID, text, "", "", replyMarkup)
	}
	if resp.StatusCode == 400 && bytes.Contains(body, []byte("can't parse entities")) {
		// Debug: log the original, escaped text, and API error to diagnose escaping issues
		origPreview := text
		if len(origPreview) > 500 {
			origPreview = origPreview[:500] + "..."
		}
		escPreview := escaped
		if len(escPreview) > 500 {
			escPreview = escPreview[:500] + "..."
		}
		log.Printf("telegram: markdown parse error, retrying as plain text\n"+
			"  API response: %s\n  Original text: %q\n  Escaped text: %q",
			string(body), origPreview, escPreview)
		v.Set("text", text)
		v.Del("parse_mode")
		resp2, err2 := retryPostForm(client, u, v)
		if err2 != nil {
			return err2
		}
		body2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		if resp2.StatusCode == 200 {
			return nil
		}
		// Also check TOPIC_CLOSED on the plain text retry
		if resp2.StatusCode == 400 && bytes.Contains(body2, []byte("TOPIC_CLOSED")) && threadID != "" {
			if dmFallbackID != "" {
				log.Printf("telegram: topic %s is closed in chat %s, falling back to DM %s", threadID, chatID, dmFallbackID)
				return tgSendMessage(client, base, dmFallbackID, text, "", "", replyMarkup)
			}
			log.Printf("telegram: topic %s is closed in chat %s, retrying without thread_id", threadID, chatID)
			return tgSendMessage(client, base, chatID, text, "", "", replyMarkup)
		}
		return fmt.Errorf("HTTP %d: %s", resp2.StatusCode, string(body2))
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
}

