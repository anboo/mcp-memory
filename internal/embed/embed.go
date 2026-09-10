// Package embed - HTTP-клиент локального embedding-сервера (Ollama).
// Батчинг + ретраи + кэш по хешу текста (storage/embeddings/<sha256>.bin).
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
	"time"
)

// Client - клиент эмбеддингов.
type Client struct {
	baseURL   string
	model     string
	http      *http.Client
	cacheDir  string
	dim       int
	keepAlive string // keep_alive для Ollama: модель выгружается через N после последнего запроса
}

// New создаёт клиент. cacheDir может быть пустым (кэш отключён).
// keepAlive: "0" - выгружать сразу, "30m" - держать 30 минут, "-1" - вечно.
func New(baseURL, model, cacheDir string, dim int) *Client {
	return NewKeepAlive(baseURL, model, cacheDir, dim, "30m")
}

// NewKeepAlive создаёт клиент с явным keep_alive.
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
	}
}

// Dim возвращает размерность векторов.
func (c *Client) Dim() int { return c.dim }

type embedRequest struct {
	Model     string   `json:"model"`
	Input     []string `json:"input"`
	KeepAlive string   `json:"keep_alive,omitempty"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error,omitempty"`
}

// Embed считает эмбеддинги батчами по batchSize.
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
			return nil, fmt.Errorf("embed: батч [%d:%d]: %w", start, end, err)
		}
		copy(out[start:end], vecs)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	// кэш: сначала проверить, что не посчитано
	missIdx := make([]int, 0, len(texts))
	for i, t := range texts {
		v, ok := c.cacheGet(t)
		if ok {
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

	body, err := json.Marshal(embedRequest{Model: c.model, Input: missTexts, KeepAlive: c.keepAlive})
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
		return nil, fmt.Errorf("ollama: вернул %d векторов на %d текстов",
			len(resp.Embeddings), len(missTexts))
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/embed", bytes.NewReader(body))
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

// ---- кэш: storage/embeddings/<sha256>.bin ----

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
		binary.LittleEndian.PutUint32(data[i*4:], float32ToBits(v))
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, data, 0o644)
}

func float32ToBits(v float32) uint32 {
	return math.Float32bits(v)
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
