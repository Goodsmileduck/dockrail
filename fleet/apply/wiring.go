package apply

import (
	"context"
	"fmt"
	"io"
)

// LogWiring is the sub-spec-4 default: it logs the intended wiring and succeeds,
// so apply is end-to-end runnable before the real drivers exist.
type LogWiring struct{ Out io.Writer }

func (w LogWiring) Apply(_ context.Context, service, backend string, endpoints []Endpoint) error {
	if w.Out != nil {
		fmt.Fprintf(w.Out, "wire %s -> %s %v (no-op: real driver in sub-spec 5)\n", service, backend, endpoints)
	}
	return nil
}
