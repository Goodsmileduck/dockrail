package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/goodsmileduck/dockrail/connection"
)

func TestAcquireLockWritesMetadataAndReleaseCleansUp(t *testing.T) {
	f := connection.NewFake()
	release, err := acquireLock(context.Background(), f, "demo", "v42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "mkdir $HOME/.dockrail/demo/lock") {
		t.Error("lock dir not created")
	}
	// Metadata is shipped base64-encoded (same idiom as writeSecretsFile);
	// decode the payload and verify the fields.
	re := regexp.MustCompile(`printf %s (\S+) \| base64 -d > \$HOME/\.dockrail/demo/lock/info\.json`)
	m := re.FindStringSubmatch(joined)
	if m == nil {
		t.Fatal("no metadata write command issued")
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatalf("metadata payload is not base64: %v", err)
	}
	var li lockInfo
	if err := json.Unmarshal(raw, &li); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if li.Tag != "v42" || li.By == "" || li.AcquiredAt == "" {
		t.Errorf("bad metadata: %+v", li)
	}
	release()
	joined = strings.Join(f.Commands, "\n")
	if !strings.Contains(joined, "rm -f $HOME/.dockrail/demo/lock/info.json && rmdir $HOME/.dockrail/demo/lock") {
		t.Error("release must remove metadata then the lock dir")
	}
}

func TestAcquireLockMetadataWriteFailureDoesNotFailAcquisition(t *testing.T) {
	f := connection.NewFake()
	f.Stub("base64 -d > $HOME/.dockrail/demo/lock/info.json", "", errors.New("disk full"))
	release, err := acquireLock(context.Background(), f, "demo", "v42")
	if err != nil {
		t.Fatalf("metadata is advisory; acquisition must succeed: %v", err)
	}
	release()
}

func TestAcquireLockCollisionReportsHolder(t *testing.T) {
	f := connection.NewFake()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	f.Stub("cat $HOME/.dockrail/demo/lock/info.json",
		`{"acquired_at":"2026-07-07T10:00:00Z","tag":"v41","by":"ci@runner"}`, nil)
	_, err := acquireLock(context.Background(), f, "demo", "v42")
	if err == nil {
		t.Fatal("want collision error")
	}
	for _, want := range []string{"held by ci@runner", "since 2026-07-07T10:00:00Z", "deploying v41"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestAcquireLockCollisionWithoutMetadata(t *testing.T) {
	f := connection.NewFake()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	f.Stub("cat $HOME/.dockrail/demo/lock/info.json", "", errors.New("No such file"))
	_, err := acquireLock(context.Background(), f, "demo", "v42")
	if err == nil || !strings.Contains(err.Error(), "no holder metadata") {
		t.Fatalf("want 'no holder metadata' in error, got %v", err)
	}
}
