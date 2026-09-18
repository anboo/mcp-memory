package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"opencode-rag/internal/search"
	"opencode-rag/internal/store"
)

type fakeStatus struct {
	st  store.Status
	err error
}

func (f fakeStatus) Status(context.Context) (store.Status, error) { return f.st, f.err }

type fakeDocs struct {
	n   uint64
	err error
}

func (f fakeDocs) DocCount() (uint64, error) { return f.n, f.err }

func TestStatusTool(t *testing.T) {
	s := &Server{
		deps: Dependencies{
			Status: fakeStatus{st: store.Status{
				Sessions: 2, Chunks: 10, Vectors: 8, PendingVectors: 2,
				EmbedDim: 1024, EmbedModel: "bge-m3", LastSync: 123, SchemaVersion: 1,
			}},
			Bleve:       fakeDocs{n: 10},
			VectorQuery: true,
		},
		lim: DefaultLimiter(),
	}
	res, err := s.handleStatus(context.Background(), mcpgo.CallToolRequest{})
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	text := textOf(t, res)

	var info StatusInfo
	if err := json.Unmarshal([]byte(text), &info); err != nil {
		t.Fatalf("unmarshal status: %v (%s)", err, text)
	}
	if info.Index.Chunks != 10 || info.Index.PendingVectors != 2 {
		t.Fatalf("unexpected counts: %+v", info.Index)
	}
	if !info.Bleve.Available || info.Bleve.Docs != 10 {
		t.Fatalf("unexpected bleve status: %+v", info.Bleve)
	}
	if !info.Source.FTS || !info.Source.Vector || !info.Source.Bleve {
		t.Fatalf("unexpected sources: %+v", info.Source)
	}
}

func TestStatusToolWithoutBleve(t *testing.T) {
	s := &Server{
		deps: Dependencies{Status: fakeStatus{st: store.Status{Chunks: 1}}},
		lim:  DefaultLimiter(),
	}
	res, err := s.handleStatus(context.Background(), mcpgo.CallToolRequest{})
	if err != nil {
		t.Fatalf("handleStatus: %v", err)
	}
	var info StatusInfo
	if err := json.Unmarshal([]byte(textOf(t, res)), &info); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if info.Bleve.Available {
		t.Fatal("bleve must be unavailable without a doc counter")
	}
}

func TestSearchEmptyResultsAreJSONArray(t *testing.T) {
	st, err := store.Open(context.Background(), t.TempDir()+"/memory.db", 4, "test")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	s := &Server{
		deps: Dependencies{Searcher: search.New(st, nil, nil)},
		lim:  DefaultLimiter(),
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "memory_search"
	req.Params.Arguments = map[string]any{"query": "zzzznomatch"}
	res, err := s.handleSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	text := textOf(t, res)
	if !strings.Contains(text, `"results":[]`) {
		t.Fatalf("empty results must be an empty array, got: %s", text)
	}
	if !strings.Contains(text, `"sources"`) {
		t.Fatalf("response must document sources: %s", text)
	}
}

func TestSearchRequiresQuery(t *testing.T) {
	st, err := store.Open(context.Background(), t.TempDir()+"/memory.db", 4, "test")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	s := &Server{deps: Dependencies{Searcher: search.New(st, nil, nil)}, lim: DefaultLimiter()}
	req := mcpgo.CallToolRequest{}
	if _, err := s.handleSearch(context.Background(), req); err == nil {
		t.Fatal("expected an error for a missing query")
	}
}

func TestArgParsing(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want int64
	}{
		{"float64", float64(7), 7},
		{"int64", int64(9), 9},
		{"int", 11, 11},
		{"string", "13", 13},
		{"json.Number", json.Number("15"), 15},
		{"invalid string", "abc", 0},
		{"invalid type", []any{1}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{"n": tc.val}
			if got := argInt(req, "n", 0); got != tc.want {
				t.Fatalf("argInt(%v) = %d, want %d", tc.val, got, tc.want)
			}
		})
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"s": "  hello  ", "n": float64(3)}
	if got := argString(req, "s"); got != "  hello  " {
		t.Fatalf("argString = %q", got)
	}
	if got := argString(req, "missing"); got != "" {
		t.Fatalf("missing string = %q", got)
	}
	if got := argString(req, "n"); got != "3" {
		t.Fatalf("numeric string = %q", got)
	}
}

func textOf(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	tc, ok := res.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("unexpected content type %T", res.Content[0])
	}
	return tc.Text
}
