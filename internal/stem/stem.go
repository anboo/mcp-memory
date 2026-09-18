// Package stem provides Russian Snowball stemming used by the SQLite FTS
// index and by query construction.
//
// FTS5's unicode61 tokenizer does not stem, so Russian morphology hurts
// recall. The index keeps a second copy of every chunk stemmed with the
// Snowball Russian algorithm, and queries are stemmed the same way before
// they hit that index.
package stem

import (
	"strings"
	"unicode"

	"github.com/kljensen/snowball"
)

// Text returns the input split into words, each stemmed with the Russian
// Snowball algorithm and lowercased, joined by single spaces.
//
// Tokenization is intentionally simple: letters, digits and underscore are
// kept, everything else is a separator.
func Text(s string) string {
	tokens := Tokenize(s)
	if len(tokens) == 0 {
		return ""
	}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, one(t))
	}
	return strings.Join(out, " ")
}

// Tokens stems already tokenized terms (used for queries).
func Tokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, one(t))
	}
	return out
}

func one(t string) string {
	st, err := snowball.Stem(t, "russian", true)
	if err != nil || st == "" {
		return t
	}
	return st
}

// Tokenize splits a string into lowercase tokens made of letters, digits and
// underscores.
func Tokenize(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, strings.ToLower(b.String()))
			b.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// Match builds an FTS5 MATCH expression from free text.
//
// Terms are quoted literals joined with AND, mirroring the old PostgreSQL
// websearch_to_tsquery baseline: tokens only contain letters, digits and
// underscore, so FTS5 operators (quotes, stars, colons, dash, dot) cannot
// leak into the expression.
//
// When stem is true the query terms are stemmed too, which is required for
// the stemmed FTS table.
func Match(text string, stemQuery bool) string {
	return FromTokens(Tokenize(text), stemQuery)
}

// FromTokens builds a MATCH expression from existing tokens.
func FromTokens(tokens []string, stemQuery bool) string {
	if len(tokens) == 0 {
		return ""
	}
	if stemQuery {
		tokens = Tokens(tokens)
	}
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if t == "" {
			continue
		}
		t = strings.ReplaceAll(t, `"`, `""`)
		parts = append(parts, `"`+t+`"`)
	}
	return strings.Join(parts, " AND ")
}
