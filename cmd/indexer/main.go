// Command indexer builds the local memory index from the OpenCode source
// database.
//
// Modes:
//
//	--all              summary of the whole database (diagnostics)
//	--session <id>     dump one session dialog (diagnostics)
//	--index            index sessions into memory.db and the Bleve index
//	--project <path>   restrict indexing to one project worktree
//	--limit N          cap the number of sessions indexed in this run
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"opencode-rag/internal/bleveidx"
	"opencode-rag/internal/config"
	"opencode-rag/internal/embed"
	"opencode-rag/internal/extract"
	"opencode-rag/internal/indexer"
	"opencode-rag/internal/store"
)

func main() {
	var (
		all        = flag.Bool("all", false, "show a summary of all sessions")
		session    = flag.String("session", "", "dump the dialog of one session")
		index      = flag.Bool("index", false, "index sessions into the local index")
		project    = flag.String("project", "", "restrict indexing to one project worktree")
		limit      = flag.Int("limit", 0, "maximum sessions to index in this run (0 = all)")
		dialogN    = flag.Int("dialog-limit", 40, "maximum parts printed in a dialog dump")
		embedPause = flag.Duration("embed-pause", 150*time.Millisecond, "pause between embedding batches")
		noBleve    = flag.Bool("no-bleve", false, "do not build or update the Bleve index")
		backfill   = flag.Bool("backfill", true, "compute vectors for chunks that have none yet")
	)
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	db, err := extract.Open(cfg.SQLitePath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	switch {
	case *index:
		runIndex(ctx, db, cfg, *project, *limit, *embedPause, *noBleve, *backfill)
	case *all:
		runSummary(ctx, db)
	case *session != "":
		runSession(ctx, db, *session, *dialogN)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// runIndex opens the local index, resolves the optional embedder and Bleve
// index, and runs one indexing pass.
func runIndex(ctx context.Context, src *sql.DB, cfg *config.Config, project string, limit int, embedPause time.Duration, noBleve, backfill bool) {
	st, err := store.Open(ctx, cfg.DBPath, cfg.EmbedDim, cfg.EmbedModel)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("index start",
		"source", cfg.SQLitePath,
		"db", cfg.DBPath,
		"bleve", cfg.BlevePath,
		"embed_model", cfg.EmbedModel,
		"embed_dim", cfg.EmbedDim,
		"limit", limit,
	)

	// Bleve is optional. A lock conflict or a broken index must not stop the
	// SQLite path.
	var bleveIndex indexer.BleveIndex
	if !noBleve {
		b, err := bleveidx.OpenOrCreate(cfg.BlevePath, cfg.StoreBleveContent)
		if err != nil {
			logger.Warn("bleve unavailable, continuing with SQLite only", "error", err.Error())
		} else {
			bleveIndex = b
			defer b.Close()
		}
	} else {
		logger.Info("bleve disabled by flag")
	}

	// Embeddings are optional too: probe once, then run FTS-only if the
	// server is not reachable.
	var emb indexer.Embedder
	client := embed.New(cfg.EmbedURL, cfg.EmbedModel, cfg.EmbedCache, cfg.EmbedDim)
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := client.Ping(probeCtx); err != nil {
		logger.Warn("embedder unavailable, indexing FTS-only and leaving vectors pending", "error", err.Error())
	} else {
		emb = client
	}
	cancel()

	rep := indexer.NewReporter(logger, 15*time.Second)
	stats, err := indexer.Run(ctx, indexer.Config{
		Source:     src,
		Store:      st,
		Bleve:      bleveIndex,
		Embed:      emb,
		Project:    project,
		Limit:      limit,
		EmbedBatch: 64,
		EmbedPause: embedPause,
		Progress:   rep,
		Backfill:   backfill,
	})
	if err != nil {
		log.Fatal(err)
	}

	chunks, _ := st.ChunkCount(ctx)
	vectors, _ := st.VectorCount(ctx)
	pending, _ := st.PendingVectorCount(ctx)
	logger.Info("index result",
		"discovered", stats.Discovered,
		"indexed", stats.Indexed,
		"skipped", stats.Skipped,
		"failed", stats.Failed,
		"chunks_written", stats.Chunks,
		"embeddings", stats.Embedded,
		"pending_vectors", pending,
		"backfilled", stats.Backfilled,
		"bleve_docs_added", stats.BleveDocs,
		"bleve_errors", stats.BleveErrors,
		"chunks_total", chunks,
		"vectors_total", vectors,
		"elapsed", stats.Elapsed.Round(time.Millisecond),
	)
}

func runSummary(ctx context.Context, db *sql.DB) {
	sessions, err := extract.ListSessions(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	projects, err := extract.ProjectMap(ctx, db)
	if err != nil {
		log.Fatal(err)
	}

	var totalParts, totalMessages int
	compacted := 0
	for _, s := range sessions {
		msgs, err := extract.ListMessages(ctx, db, s.ID)
		if err != nil {
			log.Fatal(err)
		}
		parts, err := extract.PartsInOrder(ctx, db, s.ID)
		if err != nil {
			log.Fatal(err)
		}
		totalMessages += len(msgs)
		totalParts += len(parts)
		extract.MarkCompacted(&s, parts)
		if s.Compacted {
			compacted++
		}
	}

	fmt.Printf("sessions:  %d\n", len(sessions))
	fmt.Printf("messages:  %d\n", totalMessages)
	fmt.Printf("parts:     %d\n", totalParts)
	fmt.Printf("projects:  %d\n", len(projects))
	fmt.Printf("compacted: %d\n", compacted)
	fmt.Println("--- last 5 sessions ---")
	for i := 0; i < len(sessions) && i < 5; i++ {
		s := sessions[i]
		path := projectPath(projects, s.ProjectID)
		fmt.Printf("  %s  %-16s  %-14s  %s  [%d]\n",
			shortID(s.ID), truncate(s.Title, 24), s.Agent, path, s.TimeUpdated)
	}
}

func runSession(ctx context.Context, db *sql.DB, sessionID string, limit int) {
	s, err := extract.GetSession(ctx, db, sessionID)
	if err != nil {
		log.Fatal(err)
	}
	projects, err := extract.ProjectMap(ctx, db)
	if err != nil {
		log.Fatal(err)
	}

	parts, err := extract.PartsWithPosition(ctx, db, sessionID)
	if err != nil {
		log.Fatal(err)
	}
	allParts := make([]extract.Part, len(parts))
	for i := range parts {
		allParts[i] = parts[i].Part
	}
	extract.MarkCompacted(s, allParts)

	mi := s.ModelInfo()
	fmt.Printf("session:   %s\n", s.ID)
	fmt.Printf("title:     %s\n", s.Title)
	fmt.Printf("project:   %s (%s)\n", projectPath(projects, s.ProjectID), s.ProjectID)
	fmt.Printf("agent:     %s\n", s.Agent)
	fmt.Printf("model:     %s/%s\n", mi.ProviderID, mi.ID)
	fmt.Printf("time:      %d .. %d\n", s.TimeCreated, s.TimeUpdated)
	fmt.Printf("parts:     %d\n", len(parts))
	fmt.Printf("compacted: %v (tail=%s)\n", s.Compacted, s.TailStartID)
	fmt.Println("--- dialog (first", limit, "parts) ---")

	for i, p := range parts {
		if i >= limit {
			fmt.Printf("... %d more parts\n", len(parts)-limit)
			break
		}
		fmt.Printf("[%d] %s\n", p.Position, describePart(&p.Part))
	}
}

func describePart(p *extract.Part) string {
	switch p.Type {
	case extract.PartTypeText:
		t, err := p.ParseText()
		if err != nil {
			return "text (parse error)"
		}
		return "text: " + truncate(t.Text, 100)
	case extract.PartTypeTool:
		t, err := p.ParseTool()
		if err != nil {
			return "tool (parse error)"
		}
		return fmt.Sprintf("tool %s: %s -> %s", t.Tool,
			truncate(t.State.Input.Command, 60), truncate(t.State.Output, 60))
	case extract.PartTypeReasoning:
		t, err := p.ParseReasoning()
		if err != nil {
			return "reasoning (parse error)"
		}
		return "reasoning: " + truncate(t.Text, 100)
	case extract.PartTypePatch:
		t, err := p.ParsePatch()
		if err != nil {
			return "patch (parse error)"
		}
		return fmt.Sprintf("patch %d files: %s", len(t.Files), truncate(joinFiles(t.Files), 80))
	case extract.PartTypeStepStart:
		return "step-start"
	case extract.PartTypeStepFinish:
		return "step-finish"
	case extract.PartTypeCompaction:
		c, _ := p.ParseCompaction()
		return fmt.Sprintf("compaction (tail=%s)", c.TailStartID)
	default:
		return p.Type
	}
}

func projectPath(projects map[string]string, projectID string) string {
	if p := projects[projectID]; p != "" {
		return p
	}
	return "(global)"
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

func joinFiles(files []string) string {
	out := ""
	for i, f := range files {
		if i > 0 {
			out += ", "
		}
		out += f
	}
	return out
}
