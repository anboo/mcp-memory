// indexer - индексация сессий opencode.db в PostgreSQL/pgvector.
//
// Режимы:
//
//	--all          сводка по базе (диагностика, step-1)
//	--session <id> диалог одной сессии (диагностика, step-1)
//	--index        индексация (step-2): чанкинг -> эмбеддинги -> PG
//	--project <p>  фильтр индексации по worktree
//	--limit N      максимум сессий за прогон
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"opencode-rag/internal/chunk"
	"opencode-rag/internal/config"
	"opencode-rag/internal/embed"
	"opencode-rag/internal/extract"
	"opencode-rag/internal/store"
)

func main() {
	var (
		all        = flag.Bool("all", false, "показать сводку по всем сессиям")
		session    = flag.String("session", "", "показать диалог конкретной сессии")
		index      = flag.Bool("index", false, "индексировать сессии в PostgreSQL")
		project    = flag.String("project", "", "фильтр индексации по worktree")
		limit      = flag.Int("limit", 0, "максимум сессий за прогон индексации (0 = все)")
		dialogN    = flag.Int("dialog-limit", 40, "максимум частей в выводе диалога")
		embedPause = flag.Duration("embed-pause", 150*time.Millisecond, "пауза между батчами эмбеддингов (троттлинг)")
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
		if cfg.PGDsn == "" {
			log.Fatal("index: MEMORY_PG не задан")
		}
		runIndex(ctx, db, cfg, *project, *limit, *embedPause)
	case *all:
		runSummary(ctx, db)
	case *session != "":
		runSession(ctx, db, *session, *dialogN)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// embedder - интерфейс эмбеддингов (позволяет подменять троттлинг-обёртку).
type embedder interface {
	Embed(ctx context.Context, texts []string, batchSize int) ([][]float32, error)
}

// throttledEmbedder - обёртка с паузой между батчами (не грузит машину).
type throttledEmbedder struct {
	emb   *embed.Client
	pause time.Duration
}

func (t *throttledEmbedder) Embed(ctx context.Context, texts []string, batchSize int) ([][]float32, error) {
	vecs, err := t.emb.Embed(ctx, texts, batchSize)
	if err != nil {
		return nil, err
	}
	if t.pause > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(t.pause):
		}
	}
	return vecs, nil
}

// runIndex - конвейер индексации одной сессии (arch-док B4).
func runIndex(ctx context.Context, db *sql.DB, cfg *config.Config, projectFilter string, limit int, embedPause time.Duration) {
	st, err := store.New(ctx, cfg.PGDsn)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	var emb embedder = &throttledEmbedder{
		emb:   embed.New(cfg.EmbedURL, "bge-m3", "./storage/embeddings", cfg.EmbedDim),
		pause: embedPause,
	}

	projects, err := extract.ProjectMap(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	sessions, err := extract.ListSessions(ctx, db)
	if err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	done, skipped, failed := 0, 0, 0
	for _, s := range sessions {
		if limit > 0 && done+skipped+failed >= limit {
			break
		}
		path := projects[s.ProjectID]
		if path == "" {
			path = "(global)"
		}
		if projectFilter != "" && path != projectFilter {
			continue
		}

		need, err := st.NeedsIndexing(ctx, s.ID, s.TimeUpdated)
		if err != nil {
			log.Printf("skip %s: %v", s.ID, err)
			skipped++
			continue
		}
		if !need {
			skipped++
			continue
		}

		if err := indexSession(ctx, db, st, emb, &s, path); err != nil {
			log.Printf("FAIL %s (%s): %v", s.ID, s.Title, err)
			_ = st.SetSyncState(ctx, s.ID, s.TimeUpdated, "error", err.Error())
			failed++
			continue
		}
		done++
		log.Printf("OK %s (%s) [%s]", s.ID[:12], truncate(s.Title, 40), time.Since(start).Round(time.Second))
	}

	n, _ := st.ChunkCount(ctx)
	sn, _ := st.SessionCount(ctx)
	log.Printf("готово: %d проиндексировано, %d пропущено, %d ошибок за %s",
		done, skipped, failed, time.Since(start).Round(time.Second))
	log.Printf("итог: %d чанков, %d сессий в PG", n, sn)
}

// indexSession индексирует одну сессию: чанки -> эмбеддинги -> PG.
func indexSession(ctx context.Context, db *sql.DB, st *store.Store, emb embedder,
	s *extract.Session, projectPath string) error {

	parts, err := extract.PartsWithPosition(ctx, db, s.ID)
	if err != nil {
		return err
	}
	extract.MarkCompacted(s, partsToParts(parts))

	msgs, err := extract.ListMessages(ctx, db, s.ID)
	if err != nil {
		return err
	}
	msgMap := make(map[string]extract.Message, len(msgs))
	for _, m := range msgs {
		msgMap[m.ID] = m
	}

	chunks := chunk.Chunkify(s.ID, s.ProjectID, projectPath, parts, msgMap)

	// эмбеддинги только для text/tool (B6.1), текст обрезан до MaxEmbedContentLen
	var toEmbed []string
	var embedIdx []int
	for i := range chunks {
		if chunk.NeedsEmbedding(&chunks[i]) {
			toEmbed = append(toEmbed, chunk.EmbedContent(&chunks[i]))
			embedIdx = append(embedIdx, i)
		}
	}
	if len(toEmbed) > 0 {
		vecs, err := emb.Embed(ctx, toEmbed, 64)
		if err != nil {
			return fmt.Errorf("embed: %w", err)
		}
		for j, idx := range embedIdx {
			chunks[idx].Embedding = vecs[j]
		}
	}

	if err := st.ReplaceSessionChunks(ctx, chunks); err != nil {
		return err
	}
	if err := st.UpsertSession(ctx, store.MetaFromSession(s, projectPath, len(parts))); err != nil {
		return err
	}
	if err := st.SetSyncState(ctx, s.ID, s.TimeUpdated, "indexed", ""); err != nil {
		return err
	}
	return nil
}

func partsToParts(in []extract.PartWithPos) []extract.Part {
	out := make([]extract.Part, len(in))
	for i, p := range in {
		out[i] = p.Part
	}
	return out
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

	fmt.Printf("сессий:      %d\n", len(sessions))
	fmt.Printf("сообщений:   %d\n", totalMessages)
	fmt.Printf("частей:      %d\n", totalParts)
	fmt.Printf("проектов:    %d\n", len(projects))
	fmt.Printf("скомпактировано: %d\n", compacted)
	fmt.Println("--- последние 5 сессий ---")
	for i := 0; i < len(sessions) && i < 5; i++ {
		s := sessions[i]
		path := projects[s.ProjectID]
		if path == "" {
			path = "(global)"
		}
		fmt.Printf("  %s  %-16s  %-14s  %s  [%d]\n",
			s.ID[:12], truncate(s.Title, 24), s.Agent, path, s.TimeUpdated)
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
	extract.MarkCompacted(s, partsToParts(parts))

	mi := s.ModelInfo()
	path := projects[s.ProjectID]
	fmt.Printf("сессия:    %s\n", s.ID)
	fmt.Printf("заголовок: %s\n", s.Title)
	fmt.Printf("проект:    %s (%s)\n", path, s.ProjectID)
	fmt.Printf("агент:     %s\n", s.Agent)
	fmt.Printf("модель:    %s/%s\n", mi.ProviderID, mi.ID)
	fmt.Printf("время:     %d .. %d\n", s.TimeCreated, s.TimeUpdated)
	fmt.Printf("частей:    %d\n", len(parts))
	fmt.Printf("компакция: %v (tail=%s)\n", s.Compacted, s.TailStartID)
	fmt.Println("--- диалог (первые", limit, "частей) ---")

	for i, p := range parts {
		if i >= limit {
			fmt.Printf("... ещё %d частей\n", len(parts)-limit)
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
		return fmt.Sprintf("patch %d файлов: %s", len(t.Files), truncate(joinFiles(t.Files), 80))
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
