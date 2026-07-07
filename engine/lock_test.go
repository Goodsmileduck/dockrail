package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/goodsmileduck/dockrail/config"
	"github.com/goodsmileduck/dockrail/connection"
)

func demoCfg() *config.Config {
	return &config.Config{Project: "demo", Compose: "docker-compose.yml"}
}

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

// flakyLockConn fails the lock mkdir a fixed number of times, then delegates
// to the embedded Fake. Simulates a lock freeing up mid-wait.
type flakyLockConn struct {
	*connection.Fake
	failures int
}

func (c *flakyLockConn) Run(ctx context.Context, cmd string) (string, error) {
	if strings.Contains(cmd, "mkdir $HOME/.dockrail/demo/lock") && c.failures > 0 {
		c.failures--
		return "", errors.New("File exists")
	}
	return c.Fake.Run(ctx, cmd)
}

func fastPoll(t *testing.T) {
	t.Helper()
	old := lockPollInterval
	lockPollInterval = time.Millisecond
	t.Cleanup(func() { lockPollInterval = old })
}

func TestLockWaitAcquiresWhenLockFrees(t *testing.T) {
	fastPoll(t)
	c := &flakyLockConn{Fake: connection.NewFake(), failures: 2}
	var buf bytes.Buffer
	release, err := acquireLockWait(context.Background(), c, "demo", "v42", time.Second, &buf)
	if err != nil {
		t.Fatalf("lock freed during wait; want success, got %v", err)
	}
	release()
	if !strings.Contains(buf.String(), "waiting for deploy lock") {
		t.Errorf("first collision must print a waiting line, got %q", buf.String())
	}
}

func TestLockWaitTimesOutWithHolderError(t *testing.T) {
	fastPoll(t)
	f := connection.NewFake()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	f.Stub("cat $HOME/.dockrail/demo/lock/info.json",
		`{"acquired_at":"2026-07-07T10:00:00Z","tag":"v41","by":"ci@runner"}`, nil)
	var buf bytes.Buffer
	_, err := acquireLockWait(context.Background(), f, "demo", "v42", 5*time.Millisecond, &buf)
	if err == nil || !strings.Contains(err.Error(), "held by ci@runner") {
		t.Fatalf("want holder error on timeout, got %v", err)
	}
}

func TestLockWaitZeroFailsFast(t *testing.T) {
	f := connection.NewFake()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	var buf bytes.Buffer
	_, err := acquireLockWait(context.Background(), f, "demo", "v42", 0, &buf)
	if err == nil {
		t.Fatal("wait=0 must fail immediately")
	}
	if strings.Contains(buf.String(), "waiting") {
		t.Error("wait=0 must not print a waiting line")
	}
}

func TestLockWaitShorterThanPollIntervalStillRetries(t *testing.T) {
	old := lockPollInterval
	lockPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { lockPollInterval = old })

	c := &flakyLockConn{Fake: connection.NewFake(), failures: 1}
	var buf bytes.Buffer
	start := time.Now()
	release, err := acquireLockWait(context.Background(), c, "demo", "v42", 10*time.Millisecond, &buf)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("want single retry at deadline to succeed, got %v", err)
	}
	release()
	if elapsed >= lockPollInterval {
		t.Errorf("elapsed %v should be well under poll interval %v (deadline-clamped retry)", elapsed, lockPollInterval)
	}
}

func TestLockWaitRespectsContextCancel(t *testing.T) {
	fastPoll(t)
	f := connection.NewFake()
	f.Stub("mkdir $HOME/.dockrail/demo/lock", "", errors.New("File exists"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	_, err := acquireLockWait(ctx, f, "demo", "v42", time.Minute, &buf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestLockStatusFree(t *testing.T) {
	f := connection.NewFake() // default output "" -> not "held"
	e := &Engine{Conn: f, Cfg: demoCfg(), Out: &bytes.Buffer{}}
	held, _, err := e.LockStatus(context.Background())
	if err != nil || held {
		t.Fatalf("want free, got held=%v err=%v", held, err)
	}
}

func TestLockStatusHeldWithMetadata(t *testing.T) {
	f := connection.NewFake()
	f.Stub("if test -d $HOME/.dockrail/demo/lock", "held", nil)
	f.Stub("cat $HOME/.dockrail/demo/lock/info.json",
		`{"acquired_at":"2026-07-07T10:00:00Z","tag":"v41","by":"ci@runner"}`, nil)
	e := &Engine{Conn: f, Cfg: demoCfg(), Out: &bytes.Buffer{}}
	held, desc, err := e.LockStatus(context.Background())
	if err != nil || !held {
		t.Fatalf("want held, got held=%v err=%v", held, err)
	}
	if !strings.Contains(desc, "held by ci@runner") {
		t.Errorf("desc = %q", desc)
	}
}

func TestLockReleaseWhenHeld(t *testing.T) {
	f := connection.NewFake()
	f.Stub("if test -d $HOME/.dockrail/demo/lock", "held", nil)
	e := &Engine{Conn: f, Cfg: demoCfg(), Out: &bytes.Buffer{}}
	desc, err := e.LockRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc == "" {
		t.Error("want displaced holder description")
	}
	if !strings.Contains(strings.Join(f.Commands, "\n"),
		"rm -f $HOME/.dockrail/demo/lock/info.json && rmdir $HOME/.dockrail/demo/lock") {
		t.Error("release command not issued")
	}
}

func TestLockReleaseWhenFree(t *testing.T) {
	f := connection.NewFake()
	e := &Engine{Conn: f, Cfg: demoCfg(), Out: &bytes.Buffer{}}
	desc, err := e.LockRelease(context.Background())
	if err != nil || desc != "" {
		t.Fatalf("free lock: want empty desc and nil err, got %q %v", desc, err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "rmdir") {
		t.Error("must not rmdir a free lock")
	}
}
