package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/goodsmileduck/dockrail/engine"
)

func TestCheck_ReportsMissingComposeService(t *testing.T) {
	dir := t.TempDir()
	compose := filepath.Join(dir, "docker-compose.yml")
	os.WriteFile(compose, []byte("services:\n  other:\n    image: example.com/app:v1\n"), 0o644)
	deploy := filepath.Join(dir, "deploy.yml")
	os.WriteFile(deploy, []byte(`
project: demo
compose: `+compose+`
services:
  web:
    image_tag: example.com/app:v1
    readiness: {type: tcp, port: 8080}
    cutover: {strategy: recreate}
`), 0o644)

	root := NewRootCmd()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs([]string{"check", "-c", deploy})
	err := root.Execute()
	if err == nil {
		t.Fatal("want check to fail on missing compose service")
	}
	if !bytes.Contains(errb.Bytes(), []byte(`no service "web"`)) {
		t.Fatalf("stderr missing compose validation error: %s", errb.String())
	}
}

func TestFormatDrift(t *testing.T) {
	d := engine.Drift{Service: "web", Image: "example.com/app:prod",
		Local: "sha256:aaa", Remote: "sha256:bbb", Note: "drift"}
	got := formatDrift(d)
	want := "DRIFT web example.com/app:prod host=sha256:aaa registry=sha256:bbb"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
