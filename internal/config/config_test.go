package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultSQLitePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	want := filepath.Join(home, ".local/share/opencode/opencode.db")
	if got := defaultSQLitePath(); got != want {
		t.Fatalf("defaultSQLitePath() = %q, want %q", got, want)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("MEMORY_SQLITE", "/tmp/test.db")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SQLitePath != "/tmp/test.db" {
		t.Fatalf("SQLitePath = %q", c.SQLitePath)
	}
	if c.PGDsn != "" || c.EmbedURL == "" || c.EmbedDim == 0 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadUsesDefault(t *testing.T) {
	t.Setenv("MEMORY_SQLITE", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := defaultSQLitePath()
	if c.SQLitePath != want {
		t.Fatalf("SQLitePath = %q, want default %q", c.SQLitePath, want)
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
