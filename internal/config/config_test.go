package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultDataDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	want := filepath.Join(home, ".local/share/opencode")
	if got := defaultDataDir(); got != want {
		t.Fatalf("defaultDataDir() = %q, want %q", got, want)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("MEMORY_SQLITE", "/tmp/source.db")
	t.Setenv("MEMORY_DB", "")
	t.Setenv("MEMORY_BLEVE", "")
	t.Setenv("MEMORY_EMBED_URL", "")
	t.Setenv("MEMORY_EMBED_DIM", "")
	t.Setenv("MEMORY_EMBED_MODEL", "")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SQLitePath != "/tmp/source.db" {
		t.Fatalf("SQLitePath = %q", c.SQLitePath)
	}
	if c.DBPath != filepath.Join(defaultDataDir(), "memory.db") {
		t.Fatalf("DBPath = %q", c.DBPath)
	}
	if c.BlevePath != filepath.Join(defaultDataDir(), "memory.bleve") {
		t.Fatalf("BlevePath = %q", c.BlevePath)
	}
	if c.EmbedURL != "http://localhost:11434" {
		t.Fatalf("EmbedURL = %q", c.EmbedURL)
	}
	if c.EmbedDim != 1024 {
		t.Fatalf("EmbedDim = %d", c.EmbedDim)
	}
	if c.EmbedModel != "bge-m3" {
		t.Fatalf("EmbedModel = %q", c.EmbedModel)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("MEMORY_SQLITE", "/tmp/s.db")
	t.Setenv("MEMORY_DB", "/tmp/m.db")
	t.Setenv("MEMORY_BLEVE", "/tmp/m.bleve")
	t.Setenv("MEMORY_EMBED_URL", "http://example:1234")
	t.Setenv("MEMORY_EMBED_DIM", "768")
	t.Setenv("MEMORY_EMBED_MODEL", "nomic-embed-text")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DBPath != "/tmp/m.db" || c.BlevePath != "/tmp/m.bleve" {
		t.Fatalf("paths not applied: %+v", c)
	}
	if c.EmbedURL != "http://example:1234" || c.EmbedDim != 768 || c.EmbedModel != "nomic-embed-text" {
		t.Fatalf("embed settings not applied: %+v", c)
	}
}

func TestLoadRejectsBadDim(t *testing.T) {
	t.Setenv("MEMORY_SQLITE", "/tmp/s.db")
	t.Setenv("MEMORY_EMBED_DIM", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a non-positive dimension")
	}
}

func TestGetenvInt(t *testing.T) {
	if getenvInt("MEMORY_EMBED_DIM", 768) != 768 {
		t.Fatal("missing env should return default")
	}
	t.Setenv("MEMORY_EMBED_DIM", "1024")
	if getenvInt("MEMORY_EMBED_DIM", 768) != 1024 {
		t.Fatal("parsed env should override default")
	}
	t.Setenv("MEMORY_EMBED_DIM", "abc")
	if getenvInt("MEMORY_EMBED_DIM", 768) != 768 {
		t.Fatal("invalid env should return default")
	}
}
