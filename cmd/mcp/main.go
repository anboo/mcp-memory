// mcp - MCP-сервер памяти агента.
//
// Запускается opencode через stdio. Читает opencode.db (read-only) для
// оригиналов и PostgreSQL для гибридного поиска.
//
// Конфигурация (env):
//
//	MEMORY_SQLITE    путь к opencode.db (default ~/.local/share/opencode/opencode.db)
//	MEMORY_PG        DSN PostgreSQL
//	MEMORY_EMBED_URL URL Ollama (default http://localhost:11434)
//	MEMORY_EMBED_DIM размерность модели (default 1024)
package main

import (
	"context"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"opencode-rag/internal/config"
	"opencode-rag/internal/embed"
	"opencode-rag/internal/extract"
	"opencode-rag/internal/mcp"
	"opencode-rag/internal/search"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.PGDsn == "" {
		log.Fatal("mcp: MEMORY_PG не задан")
	}

	ctx := context.Background()

	// SQLite read-only: оригиналы для memory_read/session/context
	sqlite, err := extract.Open(cfg.SQLitePath)
	if err != nil {
		log.Fatal(err)
	}
	defer sqlite.Close()

	// PostgreSQL: гибридный поиск
	pool, err := pgxpool.New(ctx, cfg.PGDsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// embedding-клиент для вектора запроса
	emb := embed.New(cfg.EmbedURL, "bge-m3", "", cfg.EmbedDim)

	searcher := search.New(pool, &queryEmbedder{emb: emb})

	srv, err := mcp.New(mcp.Dependencies{
		Searcher: searcher,
		SQLite:   sqlite,
	}, mcp.DefaultLimiter())
	if err != nil {
		log.Fatal(err)
	}

	if err := srv.ServeStdio(); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}

// queryEmbedder адаптирует embed.Client под search.VectorProvider.
type queryEmbedder struct {
	emb *embed.Client
}

func (q *queryEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := q.emb.Embed(ctx, []string{text}, 1)
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}
