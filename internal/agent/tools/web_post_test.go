package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wltechblog/gino/internal/config"
)

func TestWebPostJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "method=%s ct=%s body=%s hdr=%s", r.Method, r.Header.Get("Content-Type"), string(b), r.Header.Get("X-Custom"))
	}))
	defer srv.Close()

	tool := NewWebPostTool(5, 0, "", nil)
	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"url":     srv.URL + "/v1/thing",
		"json":    map[string]interface{}{"name": "gino", "n": 2},
		"headers": map[string]interface{}{"X-Custom": "yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "method=POST") || !strings.Contains(out, "ct=application/json") {
		t.Fatalf("method/content-type wrong: %s", out)
	}
	if !strings.Contains(out, `"name":"gino"`) {
		t.Fatalf("json body not sent: %s", out)
	}
	if !strings.Contains(out, "hdr=yes") {
		t.Fatalf("custom header not sent: %s", out)
	}
}

func TestWebPostRawBodyAndMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "method=%s body=%s", r.Method, string(b))
	}))
	defer srv.Close()

	tool := NewWebPostTool(5, 0, "", nil)
	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"url":    srv.URL + "/x",
		"method": "PUT",
		"body":   "hello=world&x=1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "method=PUT") || !strings.Contains(out, "hello=world") {
		t.Fatalf("method/body wrong: %s", out)
	}
}

func TestWebPostArgValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	tool := NewWebPostTool(5, 0, "", nil)
	ctx := context.Background()
	cases := []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{"missing url", map[string]interface{}{}, "url"},
		{"bad scheme", map[string]interface{}{"url": "ftp://x"}, "http"},
		{"bad method", map[string]interface{}{"url": srv.URL, "method": "DELETE"}, "method must be"},
		{"files+body", map[string]interface{}{
			"url": srv.URL, "body": "x",
			"files": []interface{}{map[string]interface{}{"path": "a.png"}},
		}, "cannot be combined"},
		{"body+json", map[string]interface{}{
			"url": srv.URL, "body": "x", "json": map[string]interface{}{"a": 1},
		}, "not both"},
		{"fields without files", map[string]interface{}{
			"url": srv.URL, "fields": map[string]interface{}{"a": "1"},
		}, "'fields' only applies"},
		{"files without fs", map[string]interface{}{
			"url":   srv.URL,
			"files": []interface{}{map[string]interface{}{"path": "a.png"}},
		}, "not wired"},
	}
	for _, tc := range cases {
		_, err := tool.Execute(ctx, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want err containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestWebPostTextOnlyResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0x00, 0x01, 0x02})
	}))
	defer srv.Close()

	tool := NewWebPostTool(5, 0, "", nil)
	_, err := tool.Execute(context.Background(), map[string]interface{}{"url": srv.URL})
	if err == nil || !strings.Contains(err.Error(), "content type") {
		t.Fatalf("want binary rejection, got %v", err)
	}
}

func TestWebPostMultipartUpload(t *testing.T) {
	ws := t.TempDir()
	payload := []byte("PNGDATA-pretend-binary-\x00\x01\x02")
	if err := os.WriteFile(filepath.Join(ws, "report.png"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	var gotCT, gotFilename, gotField, gotPartCT string
	var partBytes []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		for k, v := range r.MultipartForm.Value {
			if k == "extra" {
				gotField = v[0]
			}
		}
		for _, fh := range r.MultipartForm.File {
			gotFilename = fh[0].Filename
			gotPartCT = fh[0].Header.Get("Content-Type")
			if f, err := fh[0].Open(); err == nil {
				partBytes, _ = io.ReadAll(f)
				_ = f.Close()
			}
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "uploaded ok")
	}))
	defer srv.Close()

	fs, err := NewFilesystemTool(ws, nil, config.SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	tool := NewWebPostTool(10, 0, "", fs)
	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"url": srv.URL + "/upload",
		"files": []interface{}{
			map[string]interface{}{"path": "report.png", "field": "attachment", "filename": "final.png"},
		},
		"fields": map[string]interface{}{"extra": "42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "HTTP 200") || !strings.Contains(out, "uploaded ok") {
		t.Fatalf("bad response: %s", out)
	}
	if !strings.Contains(gotCT, "multipart/form-data") {
		t.Fatalf("content type: %q", gotCT)
	}
	if gotFilename != "final.png" {
		t.Fatalf("filename override: %q", gotFilename)
	}
	if gotField != "42" {
		t.Fatalf("extra field: %q", gotField)
	}
	if gotPartCT != "image/png" {
		t.Fatalf("part content type: %q", gotPartCT)
	}
	if string(partBytes) != string(payload) {
		t.Fatalf("payload mismatch: server got %q", partBytes)
	}
}

func TestWebPostMultipartSandboxRejectsOutsideFiles(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := NewFilesystemTool(ws, nil, config.SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be reached when file is outside sandbox")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tool := NewWebPostTool(5, 0, "", fs)
	_, err = tool.Execute(context.Background(), map[string]interface{}{
		"url":   srv.URL,
		"files": []interface{}{map[string]interface{}{"path": outside}},
	})
	if err == nil || !strings.Contains(err.Error(), "outside all allowed") {
		t.Fatalf("want sandbox rejection, got %v", err)
	}
}

func TestWebPostStreamsLargeFile(t *testing.T) {
	ws := t.TempDir()
	big := make([]byte, 5<<20) // 5 MB — well past any pipe buffer
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	var received int64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		mu.Lock()
		received = n
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	fs, err := NewFilesystemTool(ws, nil, config.SandboxConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	tool := NewWebPostTool(30, 0, "", fs)
	out, err := tool.Execute(context.Background(), map[string]interface{}{
		"url":   srv.URL,
		"files": []interface{}{map[string]interface{}{"path": "big.bin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := received
	mu.Unlock()
	// Raw body includes multipart framing; assert the full file made it.
	if got < int64(len(big)) {
		t.Fatalf("streamed %d bytes, want >= %d file bytes", got, len(big))
	}
	if !strings.Contains(out, "HTTP 200") {
		t.Fatalf("bad out: %s", out)
	}
}
