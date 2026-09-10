package embed

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbedBatch(t *testing.T) {
	if os.Getenv("OLLAMA_TEST") == "" {
		t.Skip("OLLAMA_TEST не задан: интеграционный тест с реальным Ollama")
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
			t.Fatalf("подозрительный вектор: %v", v[:3])
		}
	}
}

func TestCacheRoundtrip(t *testing.T) {
	dir := t.TempDir()
	c := New("", "m", dir, 4)
	vec := []float32{0.1, -0.2, 0.3, 1.5}

	c.cacheSet("текст для кэша", vec)
	got, ok := c.cacheGet("текст для кэша")
	if !ok {
		t.Fatal("кэш не найден")
	}
	if len(got) != len(vec) {
		t.Fatalf("len = %d", len(got))
	}
	for i := range vec {
		if got[i] != vec[i] {
			t.Fatalf("vec[%d] = %v, want %v", i, got[i], vec[i])
		}
	}

	// другой текст не должен попасть в кэш
	if _, ok := c.cacheGet("другой текст"); ok {
		t.Fatal("неверный хит кэша")
	}
}

func TestCacheDimMismatch(t *testing.T) {
	dir := t.TempDir()
	c := New("", "m", dir, 4)
	c.cacheSet("x", []float32{1, 2, 3, 4})

	// файл с размерностью 4, но клиент ожидает 8 -> мимо
	c2 := New("", "m", dir, 8)
	if _, ok := c2.cacheGet("x"); ok {
		t.Fatal("кэш с другой размерностью не должен срабатывать")
	}
}

func TestCacheDisabled(t *testing.T) {
	c := New("", "m", "", 4)
	if _, ok := c.cacheGet("x"); ok {
		t.Fatal("без cacheDir кэш не работает")
	}
	c.cacheSet("x", []float32{1}) // не должно паниковать
}

func TestCachePathStable(t *testing.T) {
	dir := t.TempDir()
	c := New("", "m", dir, 4)
	p1 := c.cachePath("один и тот же текст")
	p2 := c.cachePath("один и тот же текст")
	if p1 != p2 || p1 == "" {
		t.Fatalf("cachePath нестабилен: %q vs %q", p1, p2)
	}
	if filepath.Dir(p1) != dir {
		t.Fatalf("cachePath вне cacheDir: %q", p1)
	}
}
