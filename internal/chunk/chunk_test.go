package chunk

import (
	"strings"
	"testing"

	"github.com/anboo/mcp-memory/internal/extract"
)

func testPart(typ string, raw string) extract.PartWithPos {
	p := extract.PartWithPos{
		Part: extract.Part{
			ID: "prt_test", MessageID: "msg_1", SessionID: "ses_1",
			Type: typ, TimeCreated: 100, TimeUpdated: 100,
		},
		Position: 0,
	}
	p.Raw = []byte(raw)
	return p
}

func TestChunkifyText(t *testing.T) {
	// Russian content is intentional: it exercises rune-based trimming.
	part := testPart(extract.PartTypeText, `{"type":"text","text":"  привет, посмотри tree.sql  "}`)
	chunks := Chunkify("ses_1", "p1", "/var/www/test", []extract.PartWithPos{part}, nil)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	c := chunks[0]
	if c.Content != "привет, посмотри tree.sql" {
		t.Fatalf("content = %q", c.Content)
	}
	if !NeedsEmbedding(&c) {
		t.Fatal("text chunks must be embedded")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestChunkifyToolTruncation(t *testing.T) {
	big := strings.Repeat("x", MaxToolOutputLen+100)
	raw := `{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed",
		"input":{"command":"git diff"},"output":"` + big + `"}}`
	part := testPart(extract.PartTypeTool, raw)
	chunks := Chunkify("ses_1", "p1", "/var/www/test", []extract.PartWithPos{part}, nil)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	c := chunks[0]
	if !c.Truncated {
		t.Fatal("tool output must be marked truncated")
	}
	if c.Command != "git diff" || c.Tool != "bash" {
		t.Fatalf("tool/command: %q %q", c.Tool, c.Command)
	}
	if len(c.Content) > MaxToolOutputLen+100 {
		t.Fatalf("content too large: %d", len(c.Content))
	}
	if !NeedsEmbedding(&c) {
		t.Fatal("tool chunks must be embedded")
	}
}

func TestChunkifyReasoningNoEmbedding(t *testing.T) {
	part := testPart(extract.PartTypeReasoning, `{"type":"reasoning","text":"Let me check the config..."}`)
	chunks := Chunkify("ses_1", "p1", "/var/www/test", []extract.PartWithPos{part}, nil)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	if NeedsEmbedding(&chunks[0]) {
		t.Fatal("reasoning chunks must not be embedded")
	}
}

func TestChunkifyPatch(t *testing.T) {
	raw := `{"type":"patch","hash":"abc","files":["/a/b.go","/c/d.go"]}`
	part := testPart(extract.PartTypePatch, raw)
	chunks := Chunkify("ses_1", "p1", "/var/www/test", []extract.PartWithPos{part}, nil)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	c := chunks[0]
	if len(c.Files) != 2 || c.Files[0] != "/a/b.go" {
		t.Fatalf("files: %v", c.Files)
	}
	if NeedsEmbedding(&c) {
		t.Fatal("patch chunks must not be embedded")
	}
}

func TestChunkifySkipsService(t *testing.T) {
	parts := []extract.PartWithPos{
		testPart(extract.PartTypeStepStart, `{"type":"step-start","snapshot":"x"}`),
		testPart(extract.PartTypeStepFinish, `{"type":"step-finish","reason":"ok"}`),
		testPart(extract.PartTypeCompaction, `{"type":"compaction","tail_start_id":"msg_1"}`),
	}
	chunks := Chunkify("ses_1", "p1", "/var/www/test", parts, nil)
	if len(chunks) != 0 {
		t.Fatalf("service parts must not be chunked: %d", len(chunks))
	}
}

func TestChunkifyEmptyText(t *testing.T) {
	part := testPart(extract.PartTypeText, `{"type":"text","text":"   "}`)
	chunks := Chunkify("ses_1", "p1", "/var/www/test", []extract.PartWithPos{part}, nil)
	if len(chunks) != 0 {
		t.Fatal("empty text must not be chunked")
	}
}

func TestSnippet(t *testing.T) {
	// The first 6 runes of the Russian greeting are kept, then "...".
	if s := snippet("привет мир", 6); s != "привет..." {
		t.Fatalf("snippet = %q", s)
	}
	if got := snippet("короткий", 100); got != "короткий" {
		t.Fatalf("short text must not be trimmed: %q", got)
	}
}
