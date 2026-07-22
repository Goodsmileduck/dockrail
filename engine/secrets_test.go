package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

func TestWriteSecretsFileIsInjectionSafe(t *testing.T) {
	f := connection.NewFake()
	// A value that would break a naive heredoc/quoting: contains the old
	// delimiter on its own line, single quotes, and shell metacharacters.
	nasty := "line1\nDDEOF\nrm -rf ~\n'; echo pwned; '"
	prefix, err := writeSecretsFile(context.Background(), f, "demo",
		map[string]string{"APP_API_KEY": nasty})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.Commands, "\n")
	// The raw dangerous strings must never appear as shell text — they are
	// carried base64-encoded and decoded on the target.
	for _, bad := range []string{"rm -rf ~", "echo pwned", "DDEOF"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("secret content %q leaked into shell command:\n%s", bad, joined)
		}
	}
	if !strings.Contains(joined, "base64 -d") {
		t.Fatalf("expected base64 transport:\n%s", joined)
	}
	if !strings.Contains(prefix, ".dockrail/demo/env") {
		t.Fatalf("prefix must source the env-file, got %q", prefix)
	}
}

func TestWriteSecretsFileSourcesNotArgv(t *testing.T) {
	f := connection.NewFake()
	prefix, err := writeSecretsFile(context.Background(), f, "demo",
		map[string]string{"APP_API_KEY": "s3cr3t"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prefix, ".dockrail/demo/env") {
		t.Fatalf("prefix must source the env-file, got %q", prefix)
	}
	// The write happens; the secret value must not leak into the prefix that
	// gets prepended to every later command (only the file path may).
	if strings.Contains(prefix, "s3cr3t") {
		t.Fatalf("secret value leaked into command prefix: %q", prefix)
	}
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "chmod 600") {
		t.Fatalf("env-file must be chmod 600:\n%s", joined)
	}
}

func TestWriteSecretsFileEmptyIsNoop(t *testing.T) {
	f := connection.NewFake()
	prefix, err := writeSecretsFile(context.Background(), f, "demo", nil)
	if err != nil || prefix != "" {
		t.Fatalf("empty secrets must be a no-op, got prefix=%q err=%v", prefix, err)
	}
	if len(f.Commands) != 0 {
		t.Fatalf("no commands expected, got %v", f.Commands)
	}
}
