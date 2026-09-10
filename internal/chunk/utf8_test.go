package chunk

import (
	"strings"
	"testing"
	"unicode/utf8"

	"opencode-rag/internal/extract"
)

// Бинарный вывод инструмента с битой UTF-8 последовательностью.
// 0xd0 - половинка кириллического символа (реальный случай из базы).
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
			t.Fatalf("невалидный UTF-8: %q", s)
		}
	}
}

func TestToolOutputTruncateOnRuneBoundary(t *testing.T) {
	// длинный кириллический вывод: обрезка по байтам разорвала бы символ
	long := strings.Repeat("абвгд", 2000) // 10000 рун
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
		t.Fatal("должен быть truncated")
	}
	if !utf8.ValidString(c.Content) {
		t.Fatal("контент с битым UTF-8 после обрезки")
	}
	if n := len([]rune(c.Content)); n > MaxToolOutputLen+100 {
		t.Fatalf("слишком длинный: %d рун", n)
	}
}
