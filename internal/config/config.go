// Package config reads configuration from environment variables.
//
// All paths and settings have sensible defaults, so the local mode works with
// zero configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Config is the resolved service configuration.
type Config struct {
	// SQLitePath is the OpenCode source database (read-only).
	SQLitePath string
	// DBPath is the local SQLite index (memory.db). Writable.
	DBPath string
	// BlevePath is the optional Bleve index directory.
	BlevePath string
	// EmbedURL is the base URL of the embedding server (Ollama).
	EmbedURL string
	// EmbedDim is the embedding dimension; it fixes the vec0 table width.
	EmbedDim int
	// EmbedModel is the embedding model name.
	EmbedModel string
	// EmbedCache is the directory of the on-disk embedding cache.
	EmbedCache string
	// StoreBleveContent stores full content in Bleve (needed only for
	// fragments; the coordinates and snippet work without it).
	StoreBleveContent bool
}

// Load builds the configuration from the environment, applying defaults.
func Load() (*Config, error) {
	dim, err := envInt("MEMORY_EMBED_DIM", 1024)
	if err != nil {
		return nil, err
	}
	c := &Config{
		SQLitePath:        getenv("MEMORY_SQLITE", filepath.Join(defaultDataDir(), "opencode.db")),
		DBPath:            getenv("MEMORY_DB", filepath.Join(defaultDataDir(), "memory.db")),
		BlevePath:         getenv("MEMORY_BLEVE", filepath.Join(defaultDataDir(), "memory.bleve")),
		EmbedURL:          getenv("MEMORY_EMBED_URL", "http://localhost:11434"),
		EmbedDim:          dim,
		EmbedModel:        getenv("MEMORY_EMBED_MODEL", "bge-m3"),
		EmbedCache:        getenv("MEMORY_EMBED_CACHE", "./storage/embeddings"),
		StoreBleveContent: true,
	}
	if c.SQLitePath == "" {
		return nil, fmt.Errorf("config: MEMORY_SQLITE is not set and no default path is available")
	}
	if c.DBPath == "" {
		return nil, fmt.Errorf("config: MEMORY_DB is not set and no default path is available")
	}
	return c, nil
}

// defaultDataDir is the OpenCode data directory for linux.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local/share/opencode")
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	n, err := envInt(key, def)
	if err != nil {
		return def
	}
	return n
}

// envInt parses a positive integer environment variable. An empty variable
// yields def; a malformed or non-positive value is an error.
func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive integer, got %q", key, v)
	}
	return n, nil
}
