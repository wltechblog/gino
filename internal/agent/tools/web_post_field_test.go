package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/config"
)

// json passed as a STRING must not double-encode.
func TestFieldJSONStringDoubleEncoding(t *testing.T) {
	var gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	tk := NewWebPostTool(10, 1000000, "test", nil)
	if _, err := tk.Execute(context.Background(), map[string]interface{}{
		"url":  srv.URL,
		"json": `{"channel": "general", "text": "hello"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/json" {
		t.Fatalf("content type: %q", gotCT)
	}
	if gotBody != `{"channel":"general","text":"hello"}` {
		t.Fatalf("double-encoded body sent: %q", gotBody)
	}

	// body as string containing JSON should also go the JSON route
	gotBody = ""
	if _, err := tk.Execute(context.Background(), map[string]interface{}{
		"url":  srv.URL,
		"body": `{"a": 1}`,
	}); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"a":1}` {
		t.Fatalf("body-as-json-string sent: %q", gotBody)
	}

	// but a plain-text body must stay plain text
	gotBody = ""
	gotCT = ""
	if _, err := tk.Execute(context.Background(), map[string]interface{}{
		"url":  srv.URL,
		"body": `just some text {"not": "json"} trailing`,
	}); err != nil {
		t.Fatal(err)
	}
	if gotBody != `just some text {"not": "json"} trailing` || gotCT != "text/plain" && gotCT != "" {
		t.Fatalf("plain body mangled: %q ct=%q", gotBody, gotCT)
	}
}

// Bearer-token header round trip (the exact field-report use case).
func TestFieldBearerHeader(t *testing.T) {
	var gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	tk := NewWebPostTool(10, 1000000, "test", nil)
	if _, err := tk.Execute(context.Background(), map[string]interface{}{
		"url":     srv.URL,
		"headers": map[string]interface{}{"Authorization": "Bearer sk-secret-token-123"},
		"json":    map[string]interface{}{"model": "x", "input": "y"},
	}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-secret-token-123" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("content type: %q", gotCT)
	}
}

// bodyFile streams file contents as the raw request body.
func TestFieldBodyFile(t *testing.T) {
	ws := t.TempDir()
	payload := `{"huge": "payload", "items": [1,2,3]}`
	if err := os.WriteFile(filepath.Join(ws, "payload.json"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 65536)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	fs, err := NewFilesystemTool(ws, nil, config.SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	tk := NewWebPostTool(10, 0, "", fs)
	// with an explicit auth header — the field-report combo
	res, err := tk.Execute(context.Background(), map[string]interface{}{
		"url":      srv.URL,
		"bodyFile": "payload.json",
		"headers":  map[string]interface{}{"Authorization": "Bearer tok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(res, "HTTP 200") {
		t.Fatalf("res: %s", res)
	}
	if gotBody != payload {
		t.Fatalf("bodyFile bytes not streamed verbatim: %q", gotBody)
	}
	if gotCT != "application/json" {
		t.Fatalf("bodyFile content type sniff: %q", gotCT)
	}

	// sandbox rejection: file outside roots never reaches the server
	outside := filepath.Join(t.TempDir(), "evil.json")
	_ = os.WriteFile(outside, []byte(`{}`), 0o644)
	hit := false
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(200)
	}))
	defer srv2.Close()
	if _, err := tk.Execute(context.Background(), map[string]interface{}{
		"url":      srv2.URL,
		"bodyFile": outside,
	}); err == nil {
		t.Fatal("expected sandbox rejection")
	}
	if hit {
		t.Fatal("server was reached despite sandbox rejection")
	}
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }
