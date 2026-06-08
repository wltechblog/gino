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

	"github.com/local/picobot/internal/chat"
	"sync"
)

const (
	tgMaxRetries     = 3
	tgRetryBaseDelay = 2 * time.Second
)

// retryPostForm retries PostForm calls with exponential backoff.
func retryPostForm(client *http.Client, url string, data url.Values) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < tgMaxRetries; attempt++ {
		if attempt > 0 {
			delay := tgRetryBaseDelay * time.Duration(1<<(attempt-1))
			log.Printf("telegram: retry %d/%d after %v for %s", attempt, tgMaxRetries, delay, url)
			time.Sleep(delay)
		}
		resp, err := client.PostForm(url, data)
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("telegram: %d retries exhausted: %w", tgMaxRetries, lastErr)
}

// retryPost retries Post calls with exponential backoff.
func retryPost(client *http.Client, url, contentType string, body *bytes.Buffer) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < tgMaxRetries; attempt++ {
		if attempt > 0 {
			delay := tgRetryBaseDelay * time.Duration(1<<(attempt-1))
			log.Printf("telegram: retry %d/%d after %v for %s", attempt, tgMaxRetries, delay, url)
			time.Sleep(delay)
		}
		resp, err := client.Post(url, contentType, bytes.NewReader(body.Bytes()))
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("telegram: %d retries exhausted: %w", tgMaxRetries, lastErr)
}

func StartTelegram(ctx context.Context, hub *chat.Hub, token string, allowFrom []string, showTyping bool, workspace string) error {
	if token == "" {
		return fmt.Errorf("telegram token not provided")
	}
	base := "https://api.telegram.org/bot" + token
	return StartTelegramWithBase(ctx, hub, token, base, allowFrom, showTyping, workspace)
}

func StartTelegramWithBase(ctx context.Context, hub *chat.Hub, token, base string, allowFrom []string, showTyping bool, workspace string) error {
	if base == "" {
		return fmt.Errorf("base URL is required")
	}

	allowed := make(map[string]struct{}, len(allowFrom))
	for _, id := range allowFrom {
		allowed[id] = struct{}{}
	}

	client := &http.Client{Timeout: 45 * time.Second}
	fileBase := strings.Replace(base, "/bot"+token, "/file/bot"+token, 1)

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
						Text     string `json:"text"`
						Caption  string `json:"caption"`
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
					} `json:"message"`
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
				if upd.Message == nil {
					continue
				}
				m := upd.Message
				fromID := ""
				if m.From != nil {
					fromID = strconv.FormatInt(m.From.ID, 10)
				}
				if len(allowed) > 0 {
					if _, ok := allowed[fromID]; !ok {
						log.Printf("telegram: dropping message from unauthorized user %s", fromID)
						continue
					}
				}
				chatID := strconv.FormatInt(m.Chat.ID, 10)
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

			hub.In <- chat.Inbound{
				Channel:   "telegram",
				SenderID:  fromID,
				ChatID:    chatID,
				Content:   content,
				Timestamp: time.Now(),
				Media:     media,
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
				log.Printf("telegram: sending message to %s (%d chars)", out.ChatID, len(out.Content))
				if len(out.Media) > 0 {
					for i, p := range out.Media {
						caption := ""
						if i == 0 {
							caption = out.Content
						}
						if err := tgSendDocument(outClient, base, out.ChatID, p, caption); err != nil {
							log.Printf("telegram sendDocument error: %v", err)
						}
					}
					continue
				}
				u := base + "/sendMessage"
				v := url.Values{}
				v.Set("chat_id", out.ChatID)
				v.Set("text", out.Content)
				resp, err := retryPostForm(outClient, u, v)
				if err != nil {
					log.Printf("telegram sendMessage error: %v", err)
					continue
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					log.Printf("telegram sendMessage HTTP %d: %s", resp.StatusCode, string(body))
				} else {
					log.Printf("telegram: message sent successfully to %s", out.ChatID)
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

func tgSendDocument(client *http.Client, base, chatID, filePath, caption string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", chatID)
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
	w.Close()
	resp, err := retryPost(client, base+"/sendDocument", w.FormDataContentType(), &buf)
	if err != nil {
		return fmt.Errorf("sendDocument: %w", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	return nil
}
