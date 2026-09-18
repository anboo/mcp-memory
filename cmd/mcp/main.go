// Command mcp runs the memory MCP server over stdio.
//
// It reads originals from the OpenCode source database (read-only) and search
// candidates from the local SQLite index, with an optional Bleve secondary
// index. Both optional sources degrade gracefully.
//
// Configuration (environment):
//
//	MEMORY_SQLITE     path to opencode.db (default ~/.local/share/opencode/opencode.db)
//	MEMORY_DB         path to memory.db  (default ~/.local/share/opencode/memory.db)
//	MEMORY_BLEVE      path to the Bleve index directory
//	MEMORY_EMBED_URL  embedding server base URL (default http://localhost:11434)
//	MEMORY_EMBED_DIM  embedding dimension (default 1024)
//	MEMORY_EMBED_MODEL embedding model (default bge-m3)
package main

import (
	"context"
	"log"
	"os"
	"time"

	"opencode-rag/internal/bleveidx"
	"opencode-rag/internal/config"
	"opencode-rag/internal/embed"
	"opencode-rag/internal/extract"
	"opencode-rag/internal/mcp"
	"opencode-rag/internal/search"
	"opencode-rag/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	// Originals: always read-only.
	sqlite, err := extract.Open(cfg.SQLitePath)
	if err != nil {
		log.Fatal(err)
	}
	defer sqlite.Close()

	// Search index: opening creates an empty index when memory.db is absent,
	// so the server always starts.
	st, err := store.Open(ctx, cfg.DBPath, cfg.EmbedDim, cfg.EmbedModel)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	// Optional embedder. A failed probe disables vector search for this run.
	var vec search.VectorProvider
	client := embed.New(cfg.EmbedURL, cfg.EmbedModel, "", cfg.EmbedDim)
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if err := client.Ping(probeCtx); err == nil {
		vec = &queryEmbedder{client: client}
	}
	cancel()

	// Optional Bleve index. It must already exist; the MCP server never
	// builds it.
	var lex search.LexicalSource
	var bleveDocs mcp.DocCounter
	if b, err := bleveidx.Open(cfg.BlevePath); err == nil {
		lex = b
		bleveDocs = b
		defer b.Close()
	}

	searcher := search.New(st, vec, lex)

	srv, err := mcp.New(mcp.Dependencies{
		Searcher:    searcher,
		SQLite:      sqlite,
		Status:      st,
		Bleve:       bleveDocs,
		VectorQuery: vec != nil,
	}, mcp.DefaultLimiter())
	if err != nil {
		log.Fatal(err)
	}

	if err := srv.ServeStdio(); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}

// queryEmbedder adapts embed.Client to search.VectorProvider.
type queryEmbedder struct {
	client *embed.Client
}

func (q *queryEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	vecs, err := q.client.Embed(ctx, []string{text}, 1)
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}
