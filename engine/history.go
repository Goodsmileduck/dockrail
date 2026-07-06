package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goodsmileduck/dockrail/connection"
)

// Record is one line of the append-only per-project deploy history on the
// target host. Outcome is "deployed", "failed@<step>", or "rolled-back"
// (whose Tag is the tag restored).
type Record struct {
	TS        string            `json:"ts"`
	Tag       string            `json:"tag"`
	Services  map[string]string `json:"services,omitempty"`
	Performer string            `json:"performer"`
	Outcome   string            `json:"outcome"`
}

// projectDir is the per-project state directory on the target host that holds
// history, lock, logs, and the secrets env-file.
func projectDir(project string) string {
	return fmt.Sprintf("$HOME/.dockrail/%s", project)
}

func historyPath(project string) string {
	return projectDir(project) + "/history.jsonl"
}

func performer() string {
	if p := os.Getenv("DOCKRAIL_PERFORMER"); p != "" {
		return p
	}
	return os.Getenv("USER")
}

func (r Record) success() bool {
	return r.Outcome == "deployed" || r.Outcome == "rolled-back"
}

func loadHistory(ctx context.Context, conn connection.Connection, project string) ([]Record, error) {
	out, err := conn.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null || true", historyPath(project)))
	if err != nil {
		return nil, err
	}
	var h []Record
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("corrupt history line %q: %w", line, err)
		}
		h = append(h, r)
	}
	return h, nil
}

func appendRecord(ctx context.Context, conn connection.Connection, project string, r Record) error {
	if r.TS == "" {
		r.TS = time.Now().UTC().Format(time.RFC3339)
	}
	if r.Performer == "" {
		r.Performer = performer()
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	dir := projectDir(project)
	cmd := fmt.Sprintf("mkdir -p %s && cat >> %s <<'DDEOF'\n%s\nDDEOF", dir, historyPath(project), raw)
	_, err = conn.Run(ctx, cmd)
	return err
}

// currentRecord returns the last successful record — the rollback anchor.
func currentRecord(h []Record) (Record, bool) {
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].success() {
			return h[i], true
		}
	}
	return Record{}, false
}

// previousTag returns the tag of the latest successful record whose tag
// differs from the current anchor's, or "" if there is none. One backward
// pass: the first success fixes the anchor tag, the next differing-tag success
// is the answer.
func previousTag(h []Record) string {
	curTag, foundCur := "", false
	for i := len(h) - 1; i >= 0; i-- {
		if !h[i].success() {
			continue
		}
		if !foundCur {
			curTag, foundCur = h[i].Tag, true
			continue
		}
		if h[i].Tag != curTag {
			return h[i].Tag
		}
	}
	return ""
}

// lastFailure reports the trailing failure, if the most recent record is a
// failed attempt (i.e. nothing succeeded after it).
func lastFailure(h []Record) string {
	if len(h) == 0 {
		return ""
	}
	last := h[len(h)-1]
	if last.success() {
		return ""
	}
	return fmt.Sprintf("%s: %s", last.Tag, last.Outcome)
}
