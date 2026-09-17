package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"

	// регистрирует highlighter со стилем "ansi" для подсветки в терминале
	_ "github.com/blevesearch/bleve/v2/search/highlight/highlighter/ansi"
)

// SearchOpts - параметры поиска.
type SearchOpts struct {
	Text        string
	SessionID   string // фильтр по session_id (точное совпадение)
	ProjectPath string // фильтр по project_path
	PartType    string // фильтр по part_type
	TimeFrom    int64  // фильтр time_created >= TimeFrom (epoch ms), 0 = без фильтра
	Limit       int
	Mode        string // match (content+content_exact через OR) | qs (query string)
	SortTime    bool   // сортировать по свежести, а не по скору
}

// Hit - результат поиска: скор + координаты (session_id, position).
// Полный оригинал по этой координате достаёт readParts() из SQLite.
type Hit struct {
	ID          string
	Score       float64
	SessionID   string
	ProjectPath string
	PartType    string
	Role        string
	Tool        string
	Position    int
	TimeCreated int64
	Snippet     string
	Fragment    string
}

// Search выполняет запрос по локальному bleve-индексу.
func Search(idx bleve.Index, o SearchOpts) ([]Hit, error) {
	if o.Limit <= 0 {
		o.Limit = 20
	}
	if o.Limit > 200 {
		o.Limit = 200
	}

	main := textQuery(o)
	conjuncts := []query.Query{main}

	if o.SessionID != "" {
		tq := bleve.NewTermQuery(o.SessionID)
		tq.SetField("session_id")
		conjuncts = append(conjuncts, tq)
	}
	if o.ProjectPath != "" {
		tq := bleve.NewTermQuery(o.ProjectPath)
		tq.SetField("project_path")
		conjuncts = append(conjuncts, tq)
	}
	if o.PartType != "" {
		tq := bleve.NewTermQuery(o.PartType)
		tq.SetField("part_type")
		conjuncts = append(conjuncts, tq)
	}
	if o.TimeFrom > 0 {
		from := float64(o.TimeFrom)
		inc := true
		nq := bleve.NewNumericRangeInclusiveQuery(&from, nil, &inc, nil)
		nq.SetField("time_created")
		conjuncts = append(conjuncts, nq)
	}

	q := bleve.NewConjunctionQuery(conjuncts...)
	sr := bleve.NewSearchRequestOptions(q, o.Limit, 0, false)
	sr.Fields = []string{"*"} // вернуть все stored-поля, включая координаты
	h := bleve.NewHighlightWithStyle("ansi")
	h.AddField("content")
	sr.Highlight = h
	if o.SortTime {
		sr.SortBy([]string{"-time_created", "-_score"})
	}

	res, err := idx.SearchInContext(context.Background(), sr)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	out := make([]Hit, 0, len(res.Hits))
	for _, m := range res.Hits {
		out = append(out, Hit{
			ID:          m.ID,
			Score:       m.Score,
			SessionID:   strField(m.Fields, "session_id"),
			ProjectPath: strField(m.Fields, "project_path"),
			PartType:    strField(m.Fields, "part_type"),
			Role:        strField(m.Fields, "role"),
			Tool:        strField(m.Fields, "tool"),
			Position:    int(numField(m.Fields, "position")),
			TimeCreated: int64(numField(m.Fields, "time_created")),
			Snippet:     strField(m.Fields, "snippet"),
			Fragment:    firstFragment(m.Fragments, "content"),
		})
	}
	return out, nil
}

// textQuery строит запрос по режиму (--mode).
//
//	match   : OR(content ru, content_exact simple, content_ident ident)
//	          дефолт: и морфология, и точные имена, и идентификаторы
//	phrase  : точная фраза по content (ru-анализ, порядок слов)
//	ident   : terms по content_ident (идентификаторы целиком)
//	term    : один точный токен по content_ident (без анализа, привести к lower)
//	prefix  : префикс токена по content_ident (ASOC* -> ASOCVulnerability)
//	fuzzy   : нечёткий (опечатки) по content_ident, edit distance 1
//	regexp  : регулярка по content_ident (напр. CVE-20[0-9]{2}-.*)
//	wildcard: wildcard по content_ident (CCVE*DOWN, *api*key*)
//	qs      : синтаксис bleve query string (content:tree.sql, +tool:bash)
func textQuery(o SearchOpts) query.Query {
	switch o.Mode {
	case "qs":
		return bleve.NewQueryStringQuery(o.Text)
	case "phrase":
		q := bleve.NewMatchPhraseQuery(o.Text)
		q.SetField("content")
		return q
	case "ident":
		q := bleve.NewMatchQuery(o.Text)
		q.SetField("content_ident")
		return q
	case "term":
		q := bleve.NewTermQuery(strings.ToLower(o.Text))
		q.SetField("content_ident")
		return q
	case "prefix":
		q := bleve.NewPrefixQuery(strings.ToLower(o.Text))
		q.SetField("content_ident")
		return q
	case "fuzzy":
		q := bleve.NewFuzzyQuery(strings.ToLower(o.Text))
		q.SetField("content_ident")
		q.SetFuzziness(1)
		return q
	case "regexp":
		q := bleve.NewRegexpQuery(o.Text)
		q.SetField("content_ident")
		return q
	case "wildcard":
		q := bleve.NewWildcardQuery(strings.ToLower(o.Text))
		q.SetField("content_ident")
		return q
	}
	// дефолт: морфология (ru) + точные слова (simple). Идентификаторы
	// (content_ident) намеренно НЕ в дефолте: их точные совпадения
	// перегружают ранжирование, для них есть режимы ident/term/prefix/...
	m := bleve.NewMatchQuery(o.Text)
	m.SetField("content")
	e := bleve.NewMatchQuery(o.Text)
	e.SetField("content_exact")
	return bleve.NewDisjunctionQuery(m, e)
}

func strField(fields map[string]any, name string) string {
	if v, ok := fields[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func numField(fields map[string]any, name string) float64 {
	if v, ok := fields[name]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int64:
			return float64(n)
		}
	}
	return 0
}

func firstFragment(frags map[string][]string, field string) string {
	if f, ok := frags[field]; ok && len(f) > 0 {
		return f[0]
	}
	return ""
}
