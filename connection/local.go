package connection

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type Local struct{}

func NewLocal() *Local { return &Local{} }

func (l *Local) Run(ctx context.Context, cmd string) (string, error) {
	return runCmd(exec.CommandContext(ctx, "sh", "-c", cmd))
}

func runCmd(c *exec.Cmd) (string, error) {
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}
