package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"opencode-rag/internal/version"
)

// capture runs fn with stdout and stderr redirected to pipes and returns what
// was written to stdout. Stderr is captured too so tests stay quiet.
func capture(t *testing.T, fn func()) string {
	t.Helper()

	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()

	fn()

	outW.Close()
	errW.Close()
	out, _ := io.ReadAll(outR)
	io.ReadAll(errR)
	return string(out)
}

func TestRunHelp(t *testing.T) {
	var code int
	out := capture(t, func() { code = Run([]string{"help"}) })
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "Commands:") {
		t.Fatalf("help output missing commands:\n%s", out)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var code int
	capture(t, func() { code = Run([]string{"definitely-not-a-command"}) })
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunVersion(t *testing.T) {
	var code int
	out := capture(t, func() { code = Run([]string{"version"}) })
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if strings.TrimSpace(out) != version.Version {
		t.Fatalf("version output = %q, want %q", strings.TrimSpace(out), version.Version)
	}
}

func TestSessionRequiresID(t *testing.T) {
	var code int
	capture(t, func() { code = Run([]string{"session"}) })
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
