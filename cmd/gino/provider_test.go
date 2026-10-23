package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wltechblog/gino/internal/config"
)

func TestSplitProviderArgs(t *testing.T) {
	host, rest := splitProviderArgs([]string{"-key", "k", "192.168.88.13", "-no-test"}, "model", "key")
	if host != "192.168.88.13" {
		t.Fatalf("host = %q", host)
	}
	if strings.Join(rest, " ") != "-key k -no-test" {
		t.Fatalf("rest = %v", rest)
	}

	host, rest = splitProviderArgs([]string{"192.168.88.13:8080", "-key=k"}, "model", "key")
	if host != "192.168.88.13:8080" || strings.Join(rest, " ") != "-key=k" {
		t.Fatalf("got host %q rest %v", host, rest)
	}

	host, rest = splitProviderArgs([]string{"--key", "k", "host"}, "model", "key")
	if host != "host" || strings.Join(rest, " ") != "--key k" {
		t.Fatalf("got host %q rest %v", host, rest)
	}
}

func TestDiscoverCandidates(t *testing.T) {
	got, err := discoverCandidates("http://host:8080")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://host:8080/v1", "http://host:8080"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}

	got, err = discoverCandidates("http://host:8080/api")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "http://host:8080/api" {
		t.Fatalf("got %v", got)
	}

	got, err = discoverCandidates("192.168.88.13:8080")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "http://192.168.88.13:8080/v1" || got[1] != "http://192.168.88.13:8080" {
		t.Fatalf("unexpected order: %v", got)
	}

	got, err = discoverCandidates("192.168.88.13")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, "http://192.168.88.13:8080/v1") || !contains(got, "http://192.168.88.13:11434/v1") {
		t.Fatalf("missing default ports: %v", got)
	}

	if _, err := discoverCandidates("host:notaport"); err == nil {
		t.Fatal("invalid port should be rejected")
	}
}

func TestFetchModelsShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":   []map[string]string{{"id": "a"}, {"id": "b"}},
			"models": []map[string]string{{"name": "a"}, {"name": "c"}},
		})
	}))
	defer srv.Close()

	models, auth, err := fetchModels(srv.URL, "")
	if err != nil || auth {
		t.Fatalf("err=%v auth=%v", err, auth)
	}
	if strings.Join(models, ",") != "a,b,c" {
		t.Fatalf("models = %v", models)
	}
}

func TestAddProviderDiscoversSingleModel(t *testing.T) {
	srv := newProviderTestServer(t, []string{"model-a"}, "")
	defer srv.Close()

	paths := newTestPaths(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	if err := addProvider(paths, providerAddOptions{Host: host}, bufio.NewReader(strings.NewReader(""))); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.OpenAI.APIBase != srv.URL+"/v1" {
		t.Fatalf("base = %q", cfg.Providers.OpenAI.APIBase)
	}
	if cfg.Providers.OpenAI.APIKey != "" {
		t.Fatalf("key = %q, want blank", cfg.Providers.OpenAI.APIKey)
	}
	if cfg.Agents.Defaults.Model != "model-a" {
		t.Fatalf("model = %q", cfg.Agents.Defaults.Model)
	}
}

func TestAddProviderPromptsForHost(t *testing.T) {
	srv := newProviderTestServer(t, []string{"model-a"}, "")
	defer srv.Close()

	paths := newTestPaths(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	in := bufio.NewReader(strings.NewReader(host + "\n"))
	if err := addProvider(paths, providerAddOptions{}, in); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.LoadConfig(paths)
	if cfg.Agents.Defaults.Model != "model-a" {
		t.Fatalf("model = %q", cfg.Agents.Defaults.Model)
	}
}

func TestAddProviderMultipleModelsPrompts(t *testing.T) {
	srv := newProviderTestServer(t, []string{"model-a", "model-b"}, "")
	defer srv.Close()

	paths := newTestPaths(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	in := bufio.NewReader(strings.NewReader("2\n"))
	if err := addProvider(paths, providerAddOptions{Host: host}, in); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.LoadConfig(paths)
	if cfg.Agents.Defaults.Model != "model-b" {
		t.Fatalf("model = %q, want model-b", cfg.Agents.Defaults.Model)
	}
}

func TestAddProviderAsksForKeyWhenRequired(t *testing.T) {
	srv := newProviderTestServer(t, []string{"model-a"}, "secret")
	defer srv.Close()

	paths := newTestPaths(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	in := bufio.NewReader(strings.NewReader("secret\n"))
	if err := addProvider(paths, providerAddOptions{Host: host}, in); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.LoadConfig(paths)
	if cfg.Providers.OpenAI.APIKey != "secret" {
		t.Fatalf("key = %q, want secret", cfg.Providers.OpenAI.APIKey)
	}
}

func TestTestProviderChat(t *testing.T) {
	srv := newProviderTestServer(t, []string{"model-a"}, "")
	defer srv.Close()

	reply, err := testProviderChat(srv.URL+"/v1", "", "model-a")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "OK" {
		t.Fatalf("reply = %q, want OK", reply)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// newProviderTestServer serves a minimal OpenAI-compatible /v1 endpoint.
// requireKey, when set, enforces a bearer token.
func newProviderTestServer(t *testing.T, models []string, requireKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireKey != "" && r.Header.Get("Authorization") != "Bearer "+requireKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			data := make([]map[string]string, 0, len(models))
			for _, m := range models {
				data = append(data, map[string]string{"id": m})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		case "/v1/chat/completions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]string{"content": "OK"},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}
