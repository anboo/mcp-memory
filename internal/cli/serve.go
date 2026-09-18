package cli

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/anboo/mcp-memory/internal/bleveidx"
	"github.com/anboo/mcp-memory/internal/config"
	"github.com/anboo/mcp-memory/internal/embed"
	"github.com/anboo/mcp-memory/internal/extract"
	"github.com/anboo/mcp-memory/internal/mcp"
	"github.com/anboo/mcp-memory/internal/search"
	"github.com/anboo/mcp-memory/internal/store"
	"github.com/anboo/mcp-memory/internal/version"
)

// serve runs the MCP server over stdio. It reads originals from the OpenCode
// source database (read-only) and search candidates from the local index, with
// an optional Bleve secondary index. Both optional sources degrade gracefully.
func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: mcp-memory serve")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("mcp-memory " + version.Version)
		return 0
	}

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
	return 0
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
