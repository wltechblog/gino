package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClientInitializeAndListTools(t *testing.T) {
	var calls []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		calls = append(calls, req.Method)

		switch req.Method {
		case "initialize":
			resp := rpcResponse{
				JSONRPC: "2.0", ID: req.ID,
				Result: json.RawMessage(`{"capabilities":{},"serverInfo":{"name":"test"}}`),
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			resp := rpcResponse{
				JSONRPC: "2.0", ID: req.ID,
				Result: json.RawMessage(`{"tools":[{"name":"echo","description":"echoes input"}]}`),
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.Error(w, "unknown method: "+req.Method, 400)
		}
	}))
	defer srv.Close()

	client, err := NewHTTPClient("test", srv.URL, nil)
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}
	defer func() { _ = client.Close() }()

	if client.Name() != "test" {
		t.Fatalf("expected name 'test', got %q", client.Name())
	}
	tools := client.Tools()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Fatalf("expected tool 'echo', got %q", tools[0].Name)
	}
	if len(calls) < 3 {
		t.Fatalf("expected >= 3 RPC calls, got %d: %v", len(calls), calls)
	}
	if calls[0] != "initialize" {
		t.Fatalf("first call should be 'initialize', got %q", calls[0])
	}
}

func TestHTTPClientCallTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"capabilities":{}}`)})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"tools":[{"name":"greet"}]}`)})
		case "tools/call":
			b, _ := json.Marshal(req.Params)
			var p struct {
				Name      string                 `json:"name"`
				Arguments map[string]interface{} `json:"arguments"`
			}
			_ = json.Unmarshal(b, &p)
			text := "hello " + p.Arguments["name"].(string)
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"content":[{"type":"text","text":"` + text + `"}]}`)})
		}
	}))
	defer srv.Close()

	client, err := NewHTTPClient("test", srv.URL, nil)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	result, err := client.CallTool(context.TODO(), "greet", map[string]interface{}{"name": "world"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result != "hello world" {
		t.Fatalf("expected 'hello world', got %q", result)
	}
}

func TestHTTPClientCallToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"capabilities":{}}`)})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"tools":[{"name":"fail"}]}`)})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"content":[{"type":"text","text":"bad thing"}],"isError":true}`)})
		}
	}))
	defer srv.Close()

	client, err := NewHTTPClient("test", srv.URL, nil)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, err = client.CallTool(context.TODO(), "fail", nil)
	if err == nil {
		t.Fatal("expected error from isError:true response")
	}
}

func TestHTTPClientSSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "text/event-stream")
			resp, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"capabilities":{}}`)})
			_, _ = w.Write([]byte("event: message\ndata: " + string(resp) + "\n\n"))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"tools":[]}`)})
		}
	}))
	defer srv.Close()

	client, err := NewHTTPClient("sse-test", srv.URL, nil)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	if len(client.Tools()) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(client.Tools()))
	}
}

// testPNG is a minimal valid 1x1 PNG (transparent pixel).
var testPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

func TestHTTPClientCallToolImageContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"capabilities":{}}`)})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"tools":[{"name":"snap"},{"name":"plain"}]}`)})
		case "tools/call":
			var p struct {
				Name string `json:"name"`
			}
			b, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(b, &p)
			if p.Name == "plain" {
				_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"content":[{"type":"text","text":"just text"}]}`)})
				return
			}
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"content":[{"type":"text","text":"screenshot captured"},{"type":"image","data":"` + testPNG + `","mimeType":"image/png"}]}`)})
		}
	}))
	defer srv.Close()

	client, err := NewHTTPClient("test", srv.URL, nil)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	defer func() { _ = client.Close() }()

	text, imgs, err := client.CallToolWithImages(context.TODO(), "snap", nil)
	if err != nil {
		t.Fatalf("CallToolWithImages: %v", err)
	}
	if text != "screenshot captured" {
		t.Fatalf("expected text result, got %q", text)
	}
	if len(imgs) != 1 {
		t.Fatalf("expected 1 image, got %d", len(imgs))
	}
	if imgs[0].MimeType != "image/png" || imgs[0].Ext != ".png" {
		t.Fatalf("bad image metadata: %+v", imgs[0])
	}
	if len(imgs[0].Data) < 8 || imgs[0].Data[0] != 0x89 || imgs[0].Data[1] != 'P' {
		t.Fatalf("image data missing or wrong magic: %d bytes", len(imgs[0].Data))
	}

	// A follow-up text-only call returns no images.
	text, imgs, err = client.CallToolWithImages(context.TODO(), "plain", nil)
	if err != nil {
		t.Fatalf("CallToolWithImages plain: %v", err)
	}
	if text != "just text" {
		t.Fatalf("expected %q, got %q", "just text", text)
	}
	if len(imgs) != 0 {
		t.Fatalf("expected no images on text-only call, got %d", len(imgs))
	}

	// Plain CallTool still works and returns only the text.
	text, err = client.CallTool(context.TODO(), "snap", nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if text != "screenshot captured" {
		t.Fatalf("expected %q, got %q", "screenshot captured", text)
	}
}

func TestDecodeToolImageMimeSniffing(t *testing.T) {
	// Server claims JPEG but payload is PNG: magic bytes must win.
	raw, err := base64.StdEncoding.DecodeString(testPNG)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	img, err := decodeToolImage(testPNG, "image/jpeg")
	if err != nil {
		t.Fatalf("decodeToolImage: %v", err)
	}
	if img.Ext != ".png" || img.MimeType != "image/png" {
		t.Fatalf("sniffing failed: ext=%s mime=%s", img.Ext, img.MimeType)
	}
	if !bytes.Equal(img.Data, raw) {
		t.Fatal("data round-trip mismatch")
	}

	// Invalid base64 must error.
	if _, err := decodeToolImage("!!!not-base64!!!", "image/png"); err == nil {
		t.Fatal("expected error for invalid base64")
	}
}
