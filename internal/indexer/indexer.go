// Package indexer builds and incrementally updates the local memory index
// from the OpenCode source database.
//
// One pass chunks each session exactly once, writes chunks, FTS rows and
// vectors to SQLite, and streams the same chunks to a Bleve indexing
// goroutine. Embeddings are optional: when the embedder is unavailable the
// chunks are still searchable through FTS and their vectors stay pending.
package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"opencode-rag/internal/chunk"
	"opencode-rag/internal/extract"
	"opencode-rag/internal/store"
)

// Embedder computes embeddings for a batch of texts.
type Embedder interface {
	Embed(ctx context.Context, texts []string, batchSize int) ([][]float32, error)
}

// BleveIndex is the optional secondary lexical index.
type BleveIndex interface {
	AddSession(ctx context.Context, chunks []chunk.Chunk) (int, error)
	DeleteSession(ctx context.Context, sessionID string) error
	DocCount() (uint64, error)
}

// Config controls one indexing run. Store is required; Everything else is
// optional.
type Config struct {
	Source  *sql.DB
	Store   *store.Store
	Bleve   BleveIndex
	Embed   Embedder
	Project string
	Limit   int

	EmbedBatch int
	EmbedPause time.Duration

	Progress     *Reporter
	Backfill     bool
	BleveContent bool
}

// Stats summarizes one run.
type Stats struct {
	Discovered     int
	Indexed        int
	Skipped        int
	Failed         int
	Chunks         int
	Embedded       int
	PendingVectors int
	BleveDocs      int
	BleveErrors    int
	Backfilled     int
	Elapsed        time.Duration
}

// Run performs one indexing pass. Per-session failures are counted and
// recorded in sync_state; Run itself returns an error only when the source
// cannot be read.
func Run(ctx context.Context, cfg Config) (Stats, error) {
	var stats Stats
	if cfg.Store == nil {
		return stats, fmt.Errorf("indexer: store is required")
	}
	if cfg.EmbedBatch <= 0 {
		cfg.EmbedBatch = 64
	}

	projects, err := extract.ProjectMap(ctx, cfg.Source)
	if err != nil {
		return stats, err
	}
	sessions, err := extract.ListSessions(ctx, cfg.Source)
	if err != nil {
		return stats, err
	}

	queue := make([]extract.Session, 0, len(sessions))
	for _, s := range sessions {
		path := projectPath(projects, s.ProjectID)
		if cfg.Project != "" && path != cfg.Project {
			continue
		}
		queue = append(queue, s)
	}
	stats.Discovered = len(queue)

	rep := cfg.Progress
	if rep == nil {
		rep = NewReporter(nil, 0)
	}
	rep.Start(stats.Discovered)

	// Emit a periodic summary even while one long session is being processed.
	tickCtx, stopTicker := context.WithCancel(ctx)
	defer stopTicker()
	go rep.RunTicker(tickCtx)

	// Bleve consumes the same chunk stream on its own goroutine; if it is not
	// available the jobs are dropped and SQLite remains the only index.
	var bleveCh chan bleveJob
	var bleveDone chan struct{}
	if cfg.Bleve != nil {
		bleveCh = make(chan bleveJob, 4)
		bleveDone = make(chan struct{})
		go func() {
			defer close(bleveDone)
			for job := range bleveCh {
				if err := cfg.Bleve.DeleteSession(ctx, job.sessionID); err != nil {
					rep.BleveError(job.sessionID, err)
					continue
				}
				n, err := cfg.Bleve.AddSession(ctx, job.chunks)
				if err != nil {
					rep.BleveError(job.sessionID, err)
					continue
				}
				rep.AddBleveDocs(n)
			}
		}()
	}

	attempts := 0
	for i := range queue {
		if cfg.Limit > 0 && attempts >= cfg.Limit {
			break
		}
		s := queue[i]
		path := projectPath(projects, s.ProjectID)

		need, err := cfg.Store.NeedsIndexing(ctx, s.ID, s.TimeUpdated)
		if err != nil {
			stats.Skipped++
			rep.Skip()
			continue
		}
		if !need {
			stats.Skipped++
			rep.Skip()
			continue
		}
		attempts++

		res, err := indexSession(ctx, cfg, &s, path)
		stats.Chunks += res.chunks
		stats.Embedded += res.embedded
		stats.PendingVectors += res.pending
		if err != nil {
			_ = cfg.Store.SetSyncState(ctx, s.ID, s.TimeUpdated, "error", err.Error())
			stats.Failed++
			rep.Session(s.ID, s.Title, "FAIL", res.chunks, res.embedded, res.pending)
			continue
		}
		stats.Indexed++
		rep.Session(s.ID, s.Title, "ok", res.chunks, res.embedded, res.pending)

		if bleveCh != nil {
			if err := sendJob(ctx, bleveCh, bleveJob{sessionID: s.ID, chunks: res.chunkList}); err != nil {
				rep.BleveError(s.ID, err)
			}
		}
	}

	if bleveCh != nil {
		close(bleveCh)
		<-bleveDone
	}
	stats.BleveDocs = rep.BleveDocs()
	stats.BleveErrors = rep.BleveErrors()

	if cfg.Backfill && cfg.Embed != nil {
		if n, err := cfg.Store.BackfillVectors(ctx, cfg.Embed, 0); err == nil {
			stats.Backfilled = n
			stats.Embedded += n
			stats.PendingVectors = max(0, stats.PendingVectors-n)
		} else {
			rep.BackfillError(err)
		}
	}

	stats.Elapsed = rep.Elapsed()
	rep.Finish()
	return stats, nil
}

type sessionResult struct {
	chunks    int
	embedded  int
	pending   int
	chunkList []chunk.Chunk
}

// indexSession chunks one session once, writes SQLite and returns the chunks
// for the Bleve goroutine. A failed embedding is not fatal: the FTS rows are
// written and vectors stay pending.
func indexSession(ctx context.Context, cfg Config, s *extract.Session, projectPath string) (sessionResult, error) {
	var res sessionResult

	parts, err := extract.PartsWithPosition(ctx, cfg.Source, s.ID)
	if err != nil {
		return res, err
	}
	extract.MarkCompacted(s, partsToParts(parts))

	msgs, err := extract.ListMessages(ctx, cfg.Source, s.ID)
	if err != nil {
		return res, err
	}
	msgMap := make(map[string]extract.Message, len(msgs))
	for _, m := range msgs {
		msgMap[m.ID] = m
	}

	chunks := chunk.Chunkify(s.ID, s.ProjectID, projectPath, parts, msgMap)
	if len(chunks) == 0 {
		return res, cfg.Store.SetSyncState(ctx, s.ID, s.TimeUpdated, "indexed", "")
	}

	embedTexts := make(map[string]string)
	var inputs []string
	var inputIDs []string
	for i := range chunks {
		if !chunk.NeedsEmbedding(&chunks[i]) {
			continue
		}
		text := chunk.EmbedContent(&chunks[i])
		if text == "" {
			continue
		}
		embedTexts[chunks[i].ID] = text
		inputs = append(inputs, text)
		inputIDs = append(inputIDs, chunks[i].ID)
	}

	var vectors map[string][]float32
	var embedErr error
	if cfg.Embed != nil && len(inputs) > 0 {
		vecs, err := embedWithPause(ctx, cfg, inputs)
		if err != nil {
			embedErr = err
		} else if len(vecs) != len(inputs) {
			embedErr = fmt.Errorf("embedder returned %d vectors for %d texts", len(vecs), len(inputs))
		} else {
			vectors = make(map[string][]float32, len(inputIDs))
			for i, id := range inputIDs {
				vectors[id] = vecs[i]
			}
		}
	}

	// FTS and metadata are always written, even if embedding failed.
	if err := cfg.Store.ReplaceSessionChunks(ctx, chunks, embedTexts, vectors); err != nil {
		return res, err
	}
	if err := cfg.Store.UpsertSession(ctx, store.MetaFromSession(s, projectPath, len(parts))); err != nil {
		return res, err
	}

	res.chunks = len(chunks)
	res.embedded = len(vectors)
	res.pending = len(embedTexts) - len(vectors)
	res.chunkList = chunks

	if embedErr != nil {
		_ = cfg.Store.SetSyncState(ctx, s.ID, s.TimeUpdated, "indexed", "")
		return res, fmt.Errorf("embed: %w", embedErr)
	}
	if err := cfg.Store.SetSyncState(ctx, s.ID, s.TimeUpdated, "indexed", ""); err != nil {
		return res, err
	}
	return res, nil
}

func embedWithPause(ctx context.Context, cfg Config, inputs []string) ([][]float32, error) {
	vecs, err := cfg.Embed.Embed(ctx, inputs, cfg.EmbedBatch)
	if err != nil {
		return nil, err
	}
	if cfg.EmbedPause > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(cfg.EmbedPause):
		}
	}
	return vecs, nil
}

type bleveJob struct {
	sessionID string
	chunks    []chunk.Chunk
}

func sendJob(ctx context.Context, ch chan<- bleveJob, job bleveJob) error {
	select {
	case ch <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func projectPath(projects map[string]string, projectID string) string {
	if p := projects[projectID]; p != "" {
		return p
	}
	return "(global)"
}

func partsToParts(in []extract.PartWithPos) []extract.Part {
	out := make([]extract.Part, len(in))
	for i, p := range in {
		out[i] = p.Part
	}
	return out
}
