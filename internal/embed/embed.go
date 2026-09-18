// Package embed is an HTTP client for a local embedding server (Ollama).
// It batches requests, retries on failure and caches vectors on disk by text
// hash (storage/embeddings/<sha256>.bin).
package embed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultNumCtx is the context window requested from the embedding server.
// bge-m3 supports 8192 tokens; asking for it explicitly avoids the server
// defaulting to a smaller window and rejecting otherwise valid inputs.
const DefaultNumCtx = 8192

// safeRunes is the fallback cap used when a single input is rejected for
// exceeding the context length. It is deliberately conservative because the
// rune-to-token ratio is unknown (CJK text can use more than one token per
// rune).
const safeRunes = 2000

// Client is an embedding client.
type Client struct {
	baseURL   string
	model     string
	http      *http.Client
	cacheDir  string
	dim       int
	keepAlive string
	numCtx    int
}

// New creates a client. cacheDir may be empty (cache disabled).
// keepAlive: "0" unload immediately, "30m" keep 30 minutes, "-1" keep forever.
func New(baseURL, model, cacheDir string, dim int) *Client {
	return NewKeepAlive(baseURL, model, cacheDir, dim, "30m")
}

// NewKeepAlive creates a client with an explicit keep_alive.
func NewKeepAlive(baseURL, model, cacheDir string, dim int, keepAlive string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if keepAlive == "" {
		keepAlive = "30m"
	}
	return &Client{
		baseURL:   baseURL,
		model:     model,
		http:      &http.Client{Timeout: 180 * time.Second},
		cacheDir:  cacheDir,
		dim:       dim,
		keepAlive: keepAlive,
		numCtx:    DefaultNumCtx,
	}
}

// Dim returns the vector dimension.
func (c *Client) Dim() int { return c.dim }

// Model returns the model name.
func (c *Client) Model() string { return c.model }

// Ping reports whether the embedding server is reachable. It uses a short
// timeout so indexing can start in FTS-only mode quickly when the server is
// down.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("embed: ping %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("embed: ping %s: HTTP %d", c.baseURL, resp.StatusCode)
	}
	return nil
}

type embedRequest struct {
	Model     string         `json:"model"`
	Input     []string       `json:"input"`
	KeepAlive string         `json:"keep_alive,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error,omitempty"`
}

// Embed computes embeddings in batches of batchSize.
func (c *Client) Embed(ctx context.Context, texts []string, batchSize int) ([][]float32, error) {
	if batchSize <= 0 {
		batchSize = 32
	}
	out := make([][]float32, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			if !isContextLengthError(err) {
				return nil, fmt.Errorf("embed: batch [%d:%d]: %w", start, end, err)
			}
			// A batch can contain one oversized input; fall back to
			// per-input embedding and truncation instead of failing the
			// whole run (which would discard all computed vectors).
			vecs, err = c.embedIndividually(ctx, texts[start:end])
			if err != nil {
				return nil, fmt.Errorf("embed: batch [%d:%d]: %w", start, end, err)
			}
		}
		copy(out[start:end], vecs)
	}
	return out, nil
}

// embedIndividually embeds texts one at a time, truncating an input that is
// rejected for exceeding the context length until it fits.
func (c *Client) embedIndividually(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		candidate := text
		for attempt := 0; ; attempt++ {
			vecs, err := c.embedBatch(ctx, []string{candidate})
			if err == nil {
				out[i] = vecs[0]
				break
			}
			if !isContextLengthError(err) {
				return nil, err
			}
			if attempt >= 1 || len([]rune(candidate)) <= safeRunes {
				return nil, fmt.Errorf("input still exceeds the context length after truncation")
			}
			candidate = string([]rune(candidate)[:safeRunes])
		}
	}
	return out, nil
}

// isContextLengthError reports whether the server rejected an input for being
// longer than the model context.
func isContextLengthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "context length") || strings.Contains(msg, "input length")
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	missIdx := make([]int, 0, len(texts))
	for i, t := range texts {
		if v, ok := c.cacheGet(t); ok {
			vecs[i] = v
		} else {
			missIdx = append(missIdx, i)
		}
	}
	if len(missIdx) == 0 {
		return vecs, nil
	}

	missTexts := make([]string, len(missIdx))
	for j, i := range missIdx {
		missTexts[j] = texts[i]
	}

	req := embedRequest{Model: c.model, Input: missTexts, KeepAlive: c.keepAlive}
	if c.numCtx > 0 {
		req.Options = map[string]any{"num_ctx": c.numCtx}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	var resp embedResponse
	if err := c.doWithRetry(ctx, body, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("ollama: %s", resp.Error)
	}
	if len(resp.Embeddings) != len(missTexts) {
		return nil, fmt.Errorf("ollama: returned %d vectors for %d texts", len(resp.Embeddings), len(missTexts))
	}

	for j, i := range missIdx {
		vecs[i] = resp.Embeddings[j]
		c.cacheSet(texts[i], vecs[i])
	}
	return vecs, nil
}

func (c *Client) doWithRetry(ctx context.Context, body []byte, resp *embedResponse) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.doOnce(ctx, body, resp); err != nil {
			lastErr = err
			// A context-length rejection is deterministic: retrying the same
			// body cannot help and only wastes time.
			if isContextLengthError(err) {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
			continue
		}
		return nil
	}
	return lastErr
}

func (c *Client) doOnce(ctx context.Context, body []byte, resp *embedResponse) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	r, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: HTTP %d: %s", r.StatusCode, truncateBytes(data, 200))
	}
	return json.Unmarshal(data, resp)
}

// ---- cache: storage/embeddings/<sha256>.bin ----

func (c *Client) cachePath(text string) string {
	if c.cacheDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return filepath.Join(c.cacheDir, fmt.Sprintf("%x.bin", sum[:16]))
}

func (c *Client) cacheGet(text string) ([]float32, bool) {
	p := c.cachePath(text)
	if p == "" {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	n := len(data) / 4
	if n == 0 || (c.dim > 0 && n != c.dim) {
		return nil, false
	}
	vec := make([]float32, n)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return vec, true
}

func (c *Client) cacheSet(text string, vec []float32) {
	p := c.cachePath(text)
	if p == "" {
		return
	}
	data := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(data[i*4:], math.Float32bits(v))
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, data, 0o644)
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
