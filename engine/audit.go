package engine

import "context"

// Audit returns the last n history records (all if n<=0), oldest first, and
// the index within the returned slice of the current anchor (-1 if the
// anchor was truncated away or no success exists).
func (e *Engine) Audit(ctx context.Context, n int) ([]Record, int, error) {
	h, err := loadHistory(ctx, e.Conn, e.Cfg.Project)
	if err != nil {
		return nil, -1, err
	}
	cur, ok := currentRecord(h)
	if n > 0 && len(h) > n {
		h = h[len(h)-n:]
	}
	anchor := -1
	if ok {
		for i := len(h) - 1; i >= 0; i-- {
			if h[i].TS == cur.TS && h[i].Tag == cur.Tag && h[i].Outcome == cur.Outcome {
				anchor = i
				break
			}
		}
	}
	return h, anchor, nil
}
