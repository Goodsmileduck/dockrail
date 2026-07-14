package apply

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestLogWiring_LogsAndSucceeds(t *testing.T) {
	var buf bytes.Buffer
	w := LogWiring{Out: &buf}
	if err := w.Apply(context.Background(), "chat", "llama", []Endpoint{{Host: "h", Port: 8000}}); err != nil {
		t.Fatalf("LogWiring.Apply: %v", err)
	}
	if !strings.Contains(buf.String(), "chat") || !strings.Contains(buf.String(), "llama") {
		t.Fatalf("expected a wiring log line, got %q", buf.String())
	}
}
