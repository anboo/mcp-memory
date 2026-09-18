package indexer

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
)

// defaultProgressEvery is how often the periodic summary is emitted when no
// session detail line is logged.
const defaultProgressEvery = 15 * time.Second

// Reporter emits indexing progress to stderr. It is safe for concurrent use:
// the main indexing goroutine and the Bleve goroutine both update it.
type Reporter struct {
	log   *slog.Logger
	start time.Time
	every time.Duration

	mu           sync.Mutex
	total        int
	indexed      int
	skipped      int
	failed       int
	chunks       int
	embedded     int
	pending      int
	bleveDocs    int
	bleveErrors  int
	lastPeriodic time.Time
}

// NewReporter creates a reporter. A nil logger writes text logs to stderr.
func NewReporter(log *slog.Logger, every time.Duration) *Reporter {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if every <= 0 {
		every = defaultProgressEvery
	}
	return &Reporter{log: log, every: every}
}

// Start records the run start and logs the run header.
func (r *Reporter) Start(total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.start = time.Now()
	r.lastPeriodic = r.start
	r.total = total
}

// RunTicker emits a periodic summary every r.every until ctx is done. It is
// meant to run in its own goroutine so a single long session still produces
// progress updates.
func (r *Reporter) RunTicker(ctx context.Context) {
	ticker := time.NewTicker(r.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			r.maybePeriodicLocked()
			r.mu.Unlock()
		}
	}
}

// Session records a session result and logs the per-session detail line.
func (r *Reporter) Session(id, title, status string, chunks, embedded, pending int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch status {
	case "ok":
		r.indexed++
	case "FAIL":
		r.failed++
	default:
		r.skipped++
	}
	r.chunks += chunks
	r.embedded += embedded
	r.pending += pending

	r.log.Info("session indexed",
		"status", status,
		"session", shortID(id),
		"title", truncate(title, 48),
		"chunks", chunks,
		"embedded", embedded,
		"pending", pending,
		"done", r.doneLocked(),
		"total", r.total,
		"bleve_docs", r.bleveDocs,
		"rate", r.rateLocked(),
		"elapsed", r.elapsedLocked().Round(time.Millisecond),
		"eta", r.etaLocked(),
	)
	r.lastPeriodic = time.Now()
}

// Skip records a skipped session. A summary line is emitted only when the
// periodic interval has elapsed, so scanning a large up-to-date index is not
// noisy.
func (r *Reporter) Skip() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skipped++
	r.maybePeriodicLocked()
}

// AddBleveDocs adds n documents indexed by Bleve.
func (r *Reporter) AddBleveDocs(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bleveDocs += n
}

// BleveError records a Bleve write error for one session.
func (r *Reporter) BleveError(sessionID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bleveErrors++
	r.log.Warn("bleve index failed", "session", shortID(sessionID), "error", err.Error())
}

// BackfillError records a failed vector backfill.
func (r *Reporter) BackfillError(err error) {
	r.log.Warn("vector backfill failed", "error", err.Error())
}

// BleveDocs returns the number of documents added to Bleve so far.
func (r *Reporter) BleveDocs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bleveDocs
}

// BleveErrors returns the number of Bleve write errors.
func (r *Reporter) BleveErrors() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bleveErrors
}

// Elapsed returns the run duration.
func (r *Reporter) Elapsed() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.elapsedLocked()
}

// Finish logs the final summary line.
func (r *Reporter) Finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log.Info("index done",
		"indexed", r.indexed,
		"skipped", r.skipped,
		"failed", r.failed,
		"chunks", r.chunks,
		"embedded", r.embedded,
		"pending_vectors", r.pending,
		"bleve_docs", r.bleveDocs,
		"bleve_errors", r.bleveErrors,
		"elapsed", r.elapsedLocked().Round(time.Millisecond),
	)
	r.lastPeriodic = time.Now()
}

func (r *Reporter) maybePeriodicLocked() {
	if time.Since(r.lastPeriodic) < r.every {
		return
	}
	r.log.Info("index progress",
		"indexed", r.indexed,
		"skipped", r.skipped,
		"failed", r.failed,
		"done", r.doneLocked(),
		"total", r.total,
		"chunks", r.chunks,
		"embedded", r.embedded,
		"pending", r.pending,
		"bleve_docs", r.bleveDocs,
		"rate", r.rateLocked(),
		"elapsed", r.elapsedLocked().Round(time.Second),
		"eta", r.etaLocked(),
	)
	r.lastPeriodic = time.Now()
}

func (r *Reporter) doneLocked() int { return r.indexed + r.skipped + r.failed }

func (r *Reporter) elapsedLocked() time.Duration {
	if r.start.IsZero() {
		return 0
	}
	return time.Since(r.start)
}

func (r *Reporter) rateLocked() string {
	el := r.elapsedLocked().Seconds()
	if el <= 0 {
		return "0/s"
	}
	return formatRate(float64(r.doneLocked())/el) + "/s"
}

func (r *Reporter) etaLocked() string {
	done := r.doneLocked()
	el := r.elapsedLocked().Seconds()
	if done == 0 || el <= 0 || r.total <= done {
		return "0s"
	}
	remaining := float64(r.total - done)
	seconds := remaining * (el / float64(done))
	return (time.Duration(seconds) * time.Second).Round(time.Second).String()
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

func formatRate(f float64) string {
	switch {
	case f >= 10:
		return strconv.FormatFloat(f, 'f', 0, 64)
	case f >= 1:
		return strconv.FormatFloat(f, 'f', 1, 64)
	default:
		return strconv.FormatFloat(f, 'f', 2, 64)
	}
}
