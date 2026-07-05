package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

func TestCollectSecretsErrorsOnMissing(t *testing.T) {
	t.Setenv("APP_API_KEY", "abc")
	_, err := collectSecrets([]string{"APP_API_KEY", "APP_DB_CONNECTION_URL"})
	if err == nil || !strings.Contains(err.Error(), "APP_DB_CONNECTION_URL") {
		t.Fatalf("want missing-var error naming the var, got %v", err)
	}
}

func TestCollectSecretsReturnsValues(t *testing.T) {
	t.Setenv("APP_API_KEY", "abc")
	got, err := collectSecrets([]string{"APP_API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if got["APP_API_KEY"] != "abc" {
		t.Fatalf("wrong value: %v", got)
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
