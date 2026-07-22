package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
