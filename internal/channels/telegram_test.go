//go:build !only_discord && !only_slack && !only_whatsapp

package channels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wltechblog/gino/internal/chat"
)

func TestStartTelegramWithBase(t *testing.T) {
	token := "testtoken"
	sent := make(chan url.Values, 4)

	first := true
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/getUpdates") {
			w.Header().Set("Content-Type", "application/json")
			if first {
				first = false
				w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"from":{"id":123},"chat":{"id":456,"type":"private"},"text":"hello"}}]}`))
				return
			}
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		if strings.HasSuffix(path, "/sendMessage") {
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			sent <- r.PostForm
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"result":{}}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer h.Close()

	base := h.URL + "/bot" + token
	b := chat.NewHub(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := StartTelegramWithBase(ctx, b, token, base, nil, true, t.TempDir()); err != nil {
		t.Fatalf("StartTelegramWithBase failed: %v", err)
	}
	b.StartRouter(ctx)

	select {
	case msg := <-b.In:
		if msg.Content != "hello" {
			t.Fatalf("unexpected inbound content: %s", msg.Content)
		}
		if msg.ChatID != "456" {
			t.Fatalf("unexpected chat id: %s", msg.ChatID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for inbound message")
	}

	out := chat.Outbound{Channel: "telegram", ChatID: "456", Content: "reply"}
	b.Out <- out

	select {
	case v := <-sent:
		if v.Get("chat_id") != "456" || v.Get("text") != "reply" || v.Get("parse_mode") != "MarkdownV2" {
			t.Fatalf("unexpected sendMessage form: %v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for sendMessage to be posted")
	}

	cancel()
	time.Sleep(50 * time.Millisecond)
}

func TestTelegramDocumentInbound(t *testing.T) {
	token := "testtoken"
	fileContent := "hello file data"

	first := true
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/getUpdates") {
			w.Header().Set("Content-Type", "application/json")
			if first {
				first = false
				w.Write([]byte(`{"ok":true,"result":[{"update_id":2,"message":{"message_id":2,"from":{"id":123},"chat":{"id":456},"caption":"here is a file","document":{"file_id":"doc123","file_name":"test.txt"}}}]}`))
				return
			}
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		if strings.HasSuffix(path, "/getFile") {
			_ = r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"result":{"file_id":"doc123","file_path":"documents/test.txt"}}`))
			return
		}
		if strings.Contains(path, "/file/bot") {
			w.Write([]byte(fileContent))
			return
		}
		w.WriteHeader(404)
	}))
	defer h.Close()

	base := h.URL + "/bot" + token
	b := chat.NewHub(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := StartTelegramWithBase(ctx, b, token, base, nil, true, t.TempDir()); err != nil {
		t.Fatalf("StartTelegramWithBase failed: %v", err)
	}

	select {
	case msg := <-b.In:
		if !strings.Contains(msg.Content, "here is a file") {
			t.Fatalf("expected caption text in content, got: %s", msg.Content)
		}
		if !strings.Contains(msg.Content, "[File received: test.txt]") {
			t.Fatalf("expected file info in content, got: %s", msg.Content)
		}
		if len(msg.Media) != 1 {
			t.Fatalf("expected 1 media file, got %d", len(msg.Media))
		}
		data, err := os.ReadFile(msg.Media[0])
		if err != nil {
			t.Fatalf("failed to read downloaded file: %v", err)
		}
		if string(data) != fileContent {
			t.Fatalf("file content mismatch: got %q, want %q", string(data), fileContent)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for inbound document message")
	}

	cancel()
	time.Sleep(50 * time.Millisecond)
}

func TestTelegramOutboundWithMedia(t *testing.T) {
	token := "testtoken"
	sentDocs := make(chan string, 4)

	tmp := t.TempDir()
	testFile := filepath.Join(tmp, "output.txt")
	_ = os.WriteFile(testFile, []byte("output data"), 0o644)

	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/getUpdates") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		if strings.HasSuffix(path, "/sendDocument") {
			_ = r.ParseMultipartForm(10 << 20)
			sentDocs <- r.FormValue("chat_id")
			if pm := r.FormValue("parse_mode"); pm != "MarkdownV2" {
				t.Errorf("expected parse_mode=MarkdownV2, got %q", pm)
			}
			file, _, err := r.FormFile("document")
			if err != nil {
				t.Errorf("failed to read document from form: %v", err)
			} else {
				data, _ := io.ReadAll(file)
				if string(data) != "output data" {
					t.Errorf("file content mismatch: got %q", string(data))
				}
				_ = file.Close()
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true,"result":{}}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer h.Close()

	base := h.URL + "/bot" + token
	b := chat.NewHub(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := StartTelegramWithBase(ctx, b, token, base, nil, true, t.TempDir()); err != nil {
		t.Fatalf("StartTelegramWithBase failed: %v", err)
	}
	b.StartRouter(ctx)

	out := chat.Outbound{
		Channel: "telegram",
		ChatID:  "456",
		Content: "here is the result",
		Media:   []string{testFile},
	}
	b.Out <- out

	select {
	case chatID := <-sentDocs:
		if chatID != "456" {
			t.Fatalf("expected chat_id=456, got %s", chatID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for sendDocument")
	}

	cancel()
	time.Sleep(50 * time.Millisecond)
}

func TestTelegramGetFilePath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getFile") {
			_ = r.ParseForm()
			fileID := r.FormValue("file_id")
			resp := map[string]interface{}{
				"ok": true,
				"result": map[string]string{
					"file_id":   fileID,
					"file_path": "photos/file.jpg",
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	path, err := tgGetFilePath(client, srv.URL, "abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "photos/file.jpg" {
		t.Fatalf("expected photos/file.jpg, got %s", path)
	}
}
