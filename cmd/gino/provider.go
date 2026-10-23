package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wltechblog/gino/internal/config"
)

// providerAddOptions carries the non-interactive form of `provider add`.
type providerAddOptions struct {
	Host   string
	Model  string
	APIKey string
	NoTest bool
}

// probeTimeout bounds each endpoint discovery attempt.
const probeTimeout = 2500 * time.Millisecond

// defaultPorts are tried in order when only a host is given.
var defaultPorts = []string{"8080", "11434", "1234", "8000", "5000", "80", "443"}

// runProviderAdd is the `gino provider add` subcommand. Give it a host or URL;
// it discovers the port, API base, auth requirement, and model.
func runProviderAdd(paths config.Paths, args []string) {
	fs := flag.NewFlagSet("provider add", flag.ExitOnError)
	opts := providerAddOptions{}
	fs.StringVar(&opts.Model, "model", "", "Model ID (auto-selected when the endpoint serves one)")
	fs.StringVar(&opts.APIKey, "key", "", "API key (only needed when the endpoint requires one)")
	fs.BoolVar(&opts.NoTest, "no-test", false, "skip the test completion")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gino provider add [host|url] [-key KEY] [-model MODEL] [-no-test]")
		fs.PrintDefaults()
	}

	host, rest := splitProviderArgs(args, "model", "key")
	_ = fs.Parse(rest)
	opts.Host = host

	if err := addProvider(paths, opts, bufio.NewReader(os.Stdin)); err != nil {
		fmt.Fprintf(os.Stderr, "provider add: %v\n", err)
		os.Exit(1)
	}
}

// splitProviderArgs pulls the host/URL positional argument out of args so
// flags may appear before or after it. valueFlags consume the next token.
func splitProviderArgs(args []string, valueFlags ...string) (string, []string) {
	needsValue := make(map[string]bool, len(valueFlags))
	for _, f := range valueFlags {
		needsValue[f] = true
	}
	var host string
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			rest = append(rest, arg)
			if needsValue[strings.TrimLeft(arg, "-")] && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if host == "" {
			host = arg
			continue
		}
		rest = append(rest, arg)
	}
	return host, rest
}

// addProvider discovers the endpoint, picks a model, saves the provider as the
// primary one, then optionally sends a test completion.
func addProvider(paths config.Paths, opts providerAddOptions, in *bufio.Reader) error {
	if _, err := os.Stat(paths.ConfigFile()); os.IsNotExist(err) {
		if _, _, err := config.Onboard(paths); err != nil {
			return err
		}
		fmt.Printf("Initialized config at %s\n", paths.ConfigFile())
	}
	cfg, err := config.LoadConfig(paths)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	host := strings.TrimSpace(opts.Host)
	if host == "" {
		host = promptLine(in, "Host or URL: ")
	}
	if host == "" {
		return fmt.Errorf("a host or URL is required")
	}

	candidates, err := discoverCandidates(host)
	if err != nil {
		return err
	}

	key := opts.APIKey
	var base string
	var models []string
	var lastErr error
	for _, candidate := range candidates {
		found, authRequired, err := fetchModels(candidate, key)
		if authRequired && key == "" {
			key = promptLine(in, fmt.Sprintf("%s requires an API key: ", candidate))
			if key == "" {
				lastErr = fmt.Errorf("%s requires an API key", candidate)
				continue
			}
			found, authRequired, err = fetchModels(candidate, key)
		}
		if err != nil {
			lastErr = err
			continue
		}
		if authRequired {
			lastErr = fmt.Errorf("%s rejected the API key", candidate)
			continue
		}
		if len(found) == 0 {
			lastErr = fmt.Errorf("%s advertised no models", candidate)
			continue
		}
		base, models = candidate, found
		break
	}
	if base == "" {
		if lastErr != nil {
			return fmt.Errorf("no OpenAI-compatible endpoint found at %s: %w", host, lastErr)
		}
		return fmt.Errorf("no OpenAI-compatible endpoint found at %s", host)
	}
	fmt.Printf("Found provider %s\n", base)

	model := opts.Model
	if model == "" {
		if len(models) == 1 {
			model = models[0]
			fmt.Printf("Using model %s\n", filepath.Base(model))
		} else {
			model = chooseModel(in, models)
		}
	}
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("a model ID is required")
	}

	if old := cfg.Providers.OpenAI; old != nil && old.APIBase != "" && old.APIBase != base && old.APIKey != config.PlaceholderAPIKey {
		fmt.Printf("Replacing provider %s\n", old.APIBase)
	}
	cfg.Providers.OpenAI = &config.ProviderConfig{APIKey: key, APIBase: base}
	cfg.Agents.Defaults.Model = model
	cfg.Agents.Defaults.ReasoningEffort = ""
	if err := config.SaveConfig(cfg, paths.ConfigFile()); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("Saved %s\n  base:  %s\n  model: %s\n", paths.ConfigFile(), base, model)

	if !opts.NoTest {
		fmt.Println("Sending test prompt...")
		reply, err := testProviderChat(base, key, model)
		if err != nil {
			return fmt.Errorf("provider saved, but the test failed: %w", err)
		}
		fmt.Printf("Test reply: %s\n", reply)
	}
	return nil
}

// discoverCandidates turns a host, host:port, or URL into ordered API base
// candidates. Bare hosts try the common local-LLM ports.
func discoverCandidates(raw string) ([]string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("host or URL is required")
	}

	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("invalid URL %q", raw)
		}
		base := strings.TrimRight(s, "/")
		if u.Path == "" || u.Path == "/" {
			return dedupe([]string{base + "/v1", base}), nil
		}
		return []string{base}, nil
	}

	host, port, err := splitHostPort(s)
	if err != nil {
		return nil, err
	}

	var out []string
	if port != "" {
		schemes := []string{"http", "https"}
		if port == "443" {
			schemes = []string{"https", "http"}
		}
		for _, scheme := range schemes {
			base := scheme + "://" + net.JoinHostPort(host, port)
			out = append(out, base+"/v1", base)
		}
		return dedupe(out), nil
	}

	// Named hosts may be public; try TLS on the default port first. IPs and
	// localhost almost always speak plain HTTP on a high port.
	if net.ParseIP(host) == nil && host != "localhost" {
		out = append(out,
			"https://"+host+"/v1", "https://"+host,
			"http://"+host+"/v1", "http://"+host,
		)
	}
	for _, p := range defaultPorts {
		scheme := "http"
		if p == "443" {
			scheme = "https"
		}
		base := scheme + "://" + net.JoinHostPort(host, p)
		out = append(out, base+"/v1", base)
	}
	return dedupe(out), nil
}

// splitHostPort accepts host, host:port, [v6]:port, and bare IPv6.
func splitHostPort(s string) (host, port string, err error) {
	if h, p, splitErr := net.SplitHostPort(s); splitErr == nil {
		if !isDigits(p) {
			return "", "", fmt.Errorf("invalid port in %q", s)
		}
		return h, p, nil
	}
	if strings.Count(s, ":") == 1 {
		i := strings.LastIndex(s, ":")
		if i > 0 && isDigits(s[i+1:]) {
			return s[:i], s[i+1:], nil
		}
		return "", "", fmt.Errorf("invalid port in %q", s)
	}
	return s, "", nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// chooseModel prints the advertised models and accepts either an index or a
// free-form model ID.
func chooseModel(in *bufio.Reader, models []string) string {
	fmt.Println("Available models:")
	for i, m := range models {
		fmt.Printf("  %d) %s\n", i+1, filepath.Base(m))
	}
	answer := promptLine(in, fmt.Sprintf("Model [1-%d or ID] [1]: ", len(models)))
	if answer == "" {
		return models[0]
	}
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(models) {
		return models[n-1]
	}
	return answer
}

// fetchModels fetches /models and accepts both the OpenAI `data[].id` shape
// and the Ollama/llama.cpp `models[].name` shape, deduplicating the overlap.
// authRequired reports a 401/403 so the caller can ask for a key.
func fetchModels(base, apiKey string) ([]string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, false, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, true, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bodySnippet(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bodySnippet(body))
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false, fmt.Errorf("invalid model list: %w", err)
	}
	seen := make(map[string]bool)
	var models []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, id)
	}
	for _, m := range parsed.Data {
		add(m.ID)
	}
	for _, m := range parsed.Models {
		add(m.Name)
	}
	return models, false, nil
}

// testProviderChat sends one short completion to prove the endpoint and model
// actually work together.
func testProviderChat(base, apiKey, model string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with the single word: OK"},
		},
		"max_tokens": 128,
		"stream":     false,
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, bodySnippet(body))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("invalid completion response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("%s", parsed.Error.Message)
		}
		return "", fmt.Errorf("endpoint returned no choices")
	}
	msg := parsed.Choices[0].Message
	reply := strings.TrimSpace(msg.Content)
	if reply == "" {
		reply = strings.TrimSpace(msg.ReasoningContent)
	}
	if reply == "" {
		return "", fmt.Errorf("endpoint returned an empty reply")
	}
	return reply, nil
}

func bodySnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
