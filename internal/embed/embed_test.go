package embed

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbedBatch(t *testing.T) {
	if os.Getenv("OLLAMA_TEST") == "" {
		t.Skip("OLLAMA_TEST is not set: integration test against a real Ollama")
	}
	c := New("http://localhost:11434", "bge-m3", t.TempDir(), 1024)
	vecs, err := c.Embed(context.Background(), []string{"привет мир", "hello world"}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 {
		t.Fatalf("vecs = %d", len(vecs))
	}
	for _, v := range vecs {
		if len(v) != 1024 {
			t.Fatalf("dims = %d, want 1024", len(v))
		}
		if math.Abs(float64(v[0])) > 100 {
			t.Fatalf("suspicious vector: %v", v[:3])
		}
	}
}

// TestEmbedTruncatesOversizedInput covers the fallback used when the server
// rejects a batch because one input is longer than the model context.
func TestEmbedTruncatesOversizedInput(t *testing.T) {
	var sawNumCtx bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Options["num_ctx"] == float64(DefaultNumCtx) {
			sawNumCtx = true
		}
		for _, in := range req.Input {
			if len([]rune(in)) > 3000 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"the input length exceeds the context length"}`))
				return
			}
		}
		embs := make([][]float32, len(req.Input))
		for i := range embs {
			embs[i] = []float32{1, 0}
		}
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: embs})
	}))
	defer srv.Close()

	c := New(srv.URL, "m", "", 2)
	vecs, err := c.Embed(context.Background(), []string{strings.Repeat("x", 5000)}, 8)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 2 {
		t.Fatalf("vecs = %v", vecs)
	}
	if !sawNumCtx {
		t.Fatalf("request should carry options.num_ctx = %d", DefaultNumCtx)
	}
}

func TestCacheRoundtrip(t *testing.T) {
	dir := t.TempDir()
	c := New("", "m", dir, 4)
	vec := []float32{0.1, -0.2, 0.3, 1.5}

	c.cacheSet("текст для кэша", vec)
	got, ok := c.cacheGet("текст для кэша")
	if !ok {
		t.Fatal("cache miss for an entry that was just written")
	}
	if len(got) != len(vec) {
		t.Fatalf("len = %d", len(got))
	}
	for i := range vec {
		if got[i] != vec[i] {
			t.Fatalf("vec[%d] = %v, want %v", i, got[i], vec[i])
		}
	}

	// A different text must not hit the cache.
	if _, ok := c.cacheGet("другой текст"); ok {
		t.Fatal("unexpected cache hit")
	}
}

func TestCacheDimMismatch(t *testing.T) {
	dir := t.TempDir()
	c := New("", "m", dir, 4)
	c.cacheSet("x", []float32{1, 2, 3, 4})

	// The file has dimension 4 but the client expects 8: must miss.
	c2 := New("", "m", dir, 8)
	if _, ok := c2.cacheGet("x"); ok {
		t.Fatal("cache with a different dimension must not hit")
	}
}

func TestCacheDisabled(t *testing.T) {
	c := New("", "m", "", 4)
	if _, ok := c.cacheGet("x"); ok {
		t.Fatal("cache must be disabled without cacheDir")
	}
	c.cacheSet("x", []float32{1}) // must not panic
}

func TestCachePathStable(t *testing.T) {
	dir := t.TempDir()
	c := New("", "m", dir, 4)
	p1 := c.cachePath("один и тот же текст")
	p2 := c.cachePath("один и тот же текст")
	if p1 != p2 || p1 == "" {
		t.Fatalf("cachePath is not stable: %q vs %q", p1, p2)
	}
	if filepath.Dir(p1) != dir {
		t.Fatalf("cachePath outside cacheDir: %q", p1)
	}
}
