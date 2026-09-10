// Package config читает конфигурацию из переменных окружения.
package config

import (
	"fmt"
	"os"
)

// Config - конфигурация сервиса.
// На шаге 1 используется только SQLitePath; PGDsn и EmbedURL нужны с step-2.
type Config struct {
	// SQLitePath - путь к opencode.db (read-only).
	SQLitePath string
	// PGDsn - подключение к PostgreSQL (step-2).
	PGDsn string
	// EmbedURL - URL локального embedding-сервера (step-2).
	EmbedURL string
	// EmbedDim - размерность эмбеддингов модели (step-2).
	EmbedDim int
}

// Load собирает конфигурацию из окружения, применяя дефолты.
func Load() (*Config, error) {
	c := &Config{
		SQLitePath: getenv("MEMORY_SQLITE", defaultSQLitePath()),
		PGDsn:      getenv("MEMORY_PG", ""),
		EmbedURL:   getenv("MEMORY_EMBED_URL", "http://localhost:11434"),
		EmbedDim:   getenvInt("MEMORY_EMBED_DIM", 768),
	}
	if c.SQLitePath == "" {
		return nil, fmt.Errorf("config: MEMORY_SQLITE не задан и дефолтный путь не найден")
	}
	return c, nil
}

// defaultSQLitePath возвращает стандартный путь opencode для linux.
func defaultSQLitePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.local/share/opencode/opencode.db"
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n := 0
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
	}
	return n
}
