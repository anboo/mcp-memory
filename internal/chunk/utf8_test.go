package chunk

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/anboo/mcp-memory/internal/extract"
)

// Binary tool output with a broken UTF-8 sequence.
// 0xd0 is half of a Cyrillic character (a real case from the database).
func TestToolOutputInvalidUTF8(t *testing.T) {
	binOutput := "prefix\xd0\xd0\xd0binary"
	raw := `{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed",
		"input":{"command":"cat /bin/something"},"output":"` + binOutput + `"}}`
	p := extract.PartWithPos{
		Part: extract.Part{ID: "prt_x", MessageID: "m1", SessionID: "s1",
			Type: extract.PartTypeTool, TimeCreated: 1, TimeUpdated: 1},
		Position: 0,
	}
	p.Raw = []byte(raw)

	chunks := Chunkify("s1", "p1", "/", []extract.PartWithPos{p}, nil)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	c := chunks[0]
	for _, s := range []string{c.Content, c.Snippet, c.Command} {
		if !utf8.ValidString(s) {
			t.Fatalf("invalid UTF-8: %q", s)
		}
	}
}

func TestToolOutputTruncateOnRuneBoundary(t *testing.T) {
	// Long Cyrillic output: byte-based truncation would split a character.
	long := strings.Repeat("абвгд", 2000)
	raw := `{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed",
		"input":{"command":"cmd"},"output":"` + long + `"}}`
	p := extract.PartWithPos{
		Part: extract.Part{ID: "prt_x", MessageID: "m1", SessionID: "s1",
			Type: extract.PartTypeTool, TimeCreated: 1, TimeUpdated: 1},
		Position: 0,
	}
	p.Raw = []byte(raw)

	chunks := Chunkify("s1", "p1", "/", []extract.PartWithPos{p}, nil)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	c := chunks[0]
	if !c.Truncated {
		t.Fatal("must be marked truncated")
	}
	if !utf8.ValidString(c.Content) {
		t.Fatal("content is not valid UTF-8 after truncation")
	}
	if n := len([]rune(c.Content)); n > MaxToolOutputLen+100 {
		t.Fatalf("content too long: %d runes", n)
	}
}
