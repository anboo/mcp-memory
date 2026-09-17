package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"
)

const usage = `bleve-spike - локальный bleve-индекс поверх истории OpenCode.

Ничего не пишет в opencode.db: SQLite открывается read-only.

Usage:
  bleve-spike schema
  bleve-spike stat   --session SES_ID
  bleve-spike index  --session SES_ID [--index DIR]
  bleve-spike index  --all [--limit N] [--index DIR]
  bleve-spike search "QUERY" [--session S] [--type text|tool|reasoning|patch]
                     [--project PATH] [--time-from MS] [--limit N]
                     [--mode match|phrase|ident|term|prefix|fuzzy|regexp|wildcard|qs]
                     [--sort score|time] [--index DIR]
  bleve-spike read   --session S --position N [--before 5] [--after 10]
  bleve-spike ask "ВОПРОС" [--type text|tool|...] [--top 1] [--session S]
                     [--project PATH] [--before 3] [--after 5] [--index DIR]
                     # поиск + чтение оригинала вокруг лучшего совпадения

Env:
  MEMORY_SQLITE  путь к opencode.db (default ~/.local/share/opencode/opencode.db)
  MEMORY_INDEX   путь к каталогу индекса (default ./data/index.bleve)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "schema":
		err = cmdSchema(args)
	case "stat":
		err = cmdStat(args)
	case "index":
		err = cmdIndex(args)
	case "search":
		err = cmdSearch(args)
	case "read":
		err = cmdRead(args)
	case "ask":
		err = cmdAsk(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Printf("unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func sqlitePath() string {
	if v := os.Getenv("MEMORY_SQLITE"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		u, err := user.Current()
		if err == nil {
			home = u.HomeDir
		}
	}
	return home + "/.local/share/opencode/opencode.db"
}

func indexPath(def string) string {
	if v := os.Getenv("MEMORY_INDEX"); v != "" {
		return v
	}
	return def
}

func mustDB() *sql.DB {
	db, err := openDB(sqlitePath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return db
}

func cmdSchema(_ []string) error {
	ctx := context.Background()
	db := mustDB()
	defer db.Close()

	tables, err := ListTables(ctx, db)
	if err != nil {
		return err
	}
	for _, t := range tables {
		cols, err := tableColumns(ctx, db, t)
		if err != nil {
			return err
		}
		fmt.Printf("%-12s %v\n", t, cols)
	}
	return nil
}

func cmdStat(args []string) error {
	fs := flag.NewFlagSet("stat", flag.ExitOnError)
	session := fs.String("session", "", "id сессии")
	_ = fs.Parse(args)
	if *session == "" {
		return fmt.Errorf("--session обязателен")
	}

	ctx := context.Background()
	db := mustDB()
	defer db.Close()

	s, err := GetSession(ctx, db, *session)
	if err != nil {
		return err
	}
	projects, _ := ProjectMap(ctx, db)
	parts, err := PartsWithPosition(ctx, db, *session)
	if err != nil {
		return err
	}
	chunkable := 0
	byType := map[string]int{}
	for i := range parts {
		byType[parts[i].Type]++
		if _, _, _, _, ok := renderPart(&parts[i].Part, 4096); ok {
			chunkable++
		}
	}
	fmt.Printf("session:      %s\n", s.ID)
	fmt.Printf("title:        %s\n", s.Title)
	fmt.Printf("agent/model:  %s / %s\n", s.Agent, s.Model)
	fmt.Printf("project:      %s (%s)\n", s.ProjectID, projects[s.ProjectID])
	fmt.Printf("created:      %s\n", time.UnixMilli(s.TimeCreated).Format("2006-01-02 15:04:05"))
	fmt.Printf("updated:      %s\n", time.UnixMilli(s.TimeUpdated).Format("2006-01-02 15:04:05"))
	fmt.Printf("parts:        %d (в индекс пойдёт %d)\n", len(parts), chunkable)
	fmt.Printf("part types:   %v\n", byType)
	if len(parts) > 0 {
		fmt.Printf("positions:    0..%d\n", len(parts)-1)
	}
	return nil
}

func cmdIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	session := fs.String("session", "", "индексировать одну сессию")
	all := fs.Bool("all", false, "индексировать все сессии (осторожно, история большая)")
	limit := fs.Int("limit", 0, "ограничить число сессий при --all")
	dir := fs.String("index", indexPath("data/index.bleve"), "каталог индекса")
	storeContent := fs.Bool("store-content", true, "хранить полный content (нужно для подсветки)")
	_ = fs.Parse(args)
	if *session == "" && !*all {
		return fmt.Errorf("нужен --session или --all")
	}

	ctx := context.Background()
	db := mustDB()
	defer db.Close()

	idx, err := CreateIndex(*dir, *storeContent)
	if err != nil {
		return err
	}
	defer idx.Close()

	projects, _ := ProjectMap(ctx, db)

	start := time.Now()
	total := 0
	indexOne := func(id string) error {
		s, err := GetSession(ctx, db, id)
		if err != nil {
			return err
		}
		parts, err := PartsWithPosition(ctx, db, id)
		if err != nil {
			return err
		}
		roles, err := MessageRoles(ctx, db, id)
		if err != nil {
			return err
		}
		n, err := AddParts(idx, projects[s.ProjectID], parts, roles)
		if err != nil {
			return err
		}
		total += n
		fmt.Printf("  %s -> %d частей, %d документов\n", id, len(parts), n)
		return nil
	}

	if *session != "" {
		if err := indexOne(*session); err != nil {
			return err
		}
	} else {
		ids, err := ListSessionIDs(ctx, db)
		if err != nil {
			return err
		}
		if *limit > 0 && *limit < len(ids) {
			ids = ids[:*limit]
		}
		for _, id := range ids {
			if err := indexOne(id); err != nil {
				fmt.Fprintf(os.Stderr, "  пропуск %s: %v\n", id, err)
			}
		}
	}

	fmt.Printf("\nготово: %d документов за %s\n", total, time.Since(start).Round(time.Millisecond))
	fmt.Printf("индекс: %s (%s)\n", *dir, humanBytes(dirSize(*dir)))
	return nil
}

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	session := fs.String("session", "", "фильтр по session_id")
	project := fs.String("project", "", "фильтр по project_path")
	ptype := fs.String("type", "", "фильтр part_type: text|tool|reasoning|patch")
	timeFrom := fs.Int64("time-from", 0, "фильтр time_created >= (epoch ms)")
	limit := fs.Int("limit", 20, "сколько результатов")
	mode := fs.String("mode", "match", "match|phrase|ident|term|prefix|fuzzy|regexp|wildcard|qs")
	sortBy := fs.String("sort", "score", "score | time")
	dir := fs.String("index", indexPath("data/index.bleve"), "каталог индекса")
	// flag.Parse останавливается на первом не-флаге, поэтому запрос можно
	// писать и до, и после флагов: сначала вынимаем флаги, запрос - из остатка.
	flagArgs, positional := splitFlags(args)
	_ = fs.Parse(flagArgs)
	text := strings.Join(positional, " ")
	if text == "" {
		return fmt.Errorf("нужен текст запроса")
	}

	idx, err := OpenIndex(*dir)
	if err != nil {
		return err
	}
	defer idx.Close()

	hits, err := Search(idx, SearchOpts{
		Text:        text,
		SessionID:   *session,
		ProjectPath: *project,
		PartType:    *ptype,
		TimeFrom:    *timeFrom,
		Limit:       *limit,
		Mode:        *mode,
		SortTime:    *sortBy == "time",
	})
	if err != nil {
		return err
	}

	fmt.Printf("%d результатов по запросу %q\n\n", len(hits), text)
	for i, h := range hits {
		fmt.Printf("%2d. score=%.4f  %s pos=%d  [%s/%s] %s\n",
			i+1, h.Score, h.SessionID, h.Position, h.PartType, h.Role,
			time.UnixMilli(h.TimeCreated).Format("2006-01-02 15:04:05"))
		if h.Tool != "" {
			fmt.Printf("    tool=%s\n", h.Tool)
		}
		frag := h.Fragment
		if frag == "" {
			frag = h.Snippet
		}
		fmt.Printf("    %s\n\n", oneLine(frag, 240))
	}
	return nil
}

// cmdAsk - один шаг: найти по индексу и сразу прочитать оригинал вокруг
// лучшего совпадения из SQLite (search -> coords -> read).
func cmdAsk(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	session := fs.String("session", "", "фильтр по session_id")
	project := fs.String("project", "", "фильтр по project_path")
	ptype := fs.String("type", "", "фильтр part_type: text|tool|reasoning|patch")
	timeFrom := fs.Int64("time-from", 0, "фильтр time_created >= (epoch ms)")
	top := fs.Int("top", 1, "сколько лучших совпадений раскрыть")
	limit := fs.Int("limit", 10, "сколько кандидатов найти")
	mode := fs.String("mode", "match", "match | qs")
	before := fs.Int("before", 3, "частей до координаты")
	after := fs.Int("after", 5, "частей после координаты")
	dir := fs.String("index", indexPath("data/index.bleve"), "каталог индекса")
	flagArgs, positional := splitFlags(args)
	_ = fs.Parse(flagArgs)
	text := strings.Join(positional, " ")
	if text == "" {
		return fmt.Errorf("нужен текст вопроса")
	}

	idx, err := OpenIndex(*dir)
	if err != nil {
		return err
	}
	defer idx.Close()

	hits, err := Search(idx, SearchOpts{
		Text: text, SessionID: *session, ProjectPath: *project,
		PartType: *ptype, TimeFrom: *timeFrom, Limit: *limit, Mode: *mode,
	})
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("ничего не найдено")
		return nil
	}
	if *top > len(hits) {
		*top = len(hits)
	}

	ctx := context.Background()
	db := mustDB()
	defer db.Close()

	for _, h := range hits[:*top] {
		fmt.Printf("== %s pos=%d score=%.4f [%s/%s] ==\n",
			h.SessionID, h.Position, h.Score, h.PartType, h.Role)
		parts, err := readParts(ctx, db, h.SessionID, h.Position, *before, *after)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  read: %v\n", err)
			continue
		}
		for i := range parts {
			p := &parts[i]
			mark := " "
			if p.Position == h.Position {
				mark = "*"
			}
			content := renderFull(&p.Part, 0)
			if content == "" {
				continue
			}
			fmt.Printf("%s pos=%d [%s] %s\n", mark, p.Position, p.Type, p.ID)
			fmt.Printf("    %s\n\n", indent(content))
		}
	}
	return nil
}

func cmdRead(args []string) error {
	fs := flag.NewFlagSet("read", flag.ExitOnError)
	session := fs.String("session", "", "id сессии")
	pos := fs.Int("position", -1, "координата внутри сессии")
	before := fs.Int("before", 3, "частей до")
	after := fs.Int("after", 5, "частей после")
	_ = fs.Parse(args)
	if *session == "" || *pos < 0 {
		return fmt.Errorf("нужны --session и --position")
	}

	ctx := context.Background()
	db := mustDB()
	defer db.Close()

	parts, err := readParts(ctx, db, *session, *pos, *before, *after)
	if err != nil {
		return err
	}
	for i := range parts {
		p := &parts[i]
		mark := " "
		if p.Position == *pos {
			mark = "*"
		}
		content := renderFull(&p.Part, 0) // без обрезки: индекс для координат, оригинал здесь
		fmt.Printf("%s pos=%d [%s] %s\n", mark, p.Position, p.Type, p.ID)
		if content == "" {
			fmt.Printf("    (нет текста)\n\n")
			continue
		}
		fmt.Printf("    %s\n\n", indent(content))
	}
	return nil
}

// splitFlags отделяет флаги (со значениями) от позиционных аргументов так,
// что запрос можно передавать в любой позиции. Все флаги search принимают
// значение, поэтому пара "флаг значение" восстанавливается однозначно.
func splitFlags(args []string) (flagArgs, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
			continue
		}
		positional = append(positional, a)
	}
	return flagArgs, positional
}

func oneLine(s string, max int) string {
	r := []rune(s)
	for i := range r {
		if r[i] == '\n' {
			r[i] = ' '
		}
	}
	if len(r) > max {
		return string(r[:max]) + "..."
	}
	return string(r)
}

func indent(s string) string {
	out := make([]rune, 0, len(s))
	lineStart := true
	for _, r := range s {
		if lineStart {
			out = append(out, ' ', ' ')
			lineStart = false
		}
		out = append(out, r)
		if r == '\n' {
			lineStart = true
		}
	}
	return string(out)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
