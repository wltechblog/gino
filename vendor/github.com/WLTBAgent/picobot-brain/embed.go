package brain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OllamaProvider computes embeddings via a local Ollama instance.
// Works with nomic-embed-text, mxbai-embed-large, bge-m3, etc.
type OllamaProvider struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

// OllamaConfig configures the Ollama embedding provider.
type OllamaConfig struct {
	BaseURL string // default: "http://localhost:11434"
	Model   string // default: "nomic-embed-text"
	Timeout time.Duration // default: 30s
}

// NewOllamaProvider creates an embedding provider backed by a local Ollama instance.
func NewOllamaProvider(cfg OllamaConfig) *OllamaProvider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:11434"
	}
	if cfg.Model == "" {
		cfg.Model = "nomic-embed-text"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &OllamaProvider{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   cfg.Model,
		httpClient: &http.Client{Timeout: cfg.Timeout},
	}
}

func (o *OllamaProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := o.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("ollama: no embedding returned")
	}
	return vecs[0], nil
}

func (o *OllamaProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	// Ollama's /api/embeddings endpoint (also supports OpenAI-compatible /v1/embeddings)
	reqBody := map[string]any{
		"model": o.model,
		"input": texts,
	}
	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embedding: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embedding: status %d: %s", resp.StatusCode, string(b))
	}

	// Parse OpenAI-compatible response
	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama embedding: decode: %w", err)
	}

	vecs := make([][]float32, 0, len(result.Data))
	for _, d := range result.Data {
		vecs = append(vecs, d.Embedding)
	}
	return vecs, nil
}

func (o *OllamaProvider) ModelName() string { return o.model }

// RemoteAPIProvider computes embeddings via an OpenAI-compatible remote API.
// Works with OpenAI, Voyage, Anthropic, or any compatible endpoint.
type RemoteAPIProvider struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

// RemoteAPIConfig configures a remote embedding API.
type RemoteAPIConfig struct {
	BaseURL string // e.g., "https://api.openai.com"
	APIKey  string
	Model   string // e.g., "text-embedding-3-small"
	Timeout time.Duration
}

// NewRemoteAPIProvider creates an embedding provider backed by a remote API.
func NewRemoteAPIProvider(cfg RemoteAPIConfig) *RemoteAPIProvider {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &RemoteAPIProvider{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		httpClient: &http.Client{Timeout: cfg.Timeout},
	}
}

func (r *RemoteAPIProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := r.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("remote api: no embedding returned")
	}
	return vecs[0], nil
}

func (r *RemoteAPIProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	reqBody := map[string]any{
		"model": r.model,
		"input": texts,
	}
	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, "POST", r.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("remote embedding: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("remote embedding: status %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("remote embedding: decode: %w", err)
	}

	vecs := make([][]float32, 0, len(result.Data))
	for _, d := range result.Data {
		vecs = append(vecs, d.Embedding)
	}
	return vecs, nil
}

func (r *RemoteAPIProvider) ModelName() string { return r.model }
