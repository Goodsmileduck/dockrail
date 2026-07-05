package connection

import (
	"context"
	"fmt"
	"os/exec"
)

type SSH struct {
	host string
	port int
}

func NewSSH(host string, port int) *SSH {
	if port == 0 {
		port = 22
	}
	return &SSH{host: host, port: port}
}

func (s *SSH) sshArgs(cmd string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=~/.ssh/dockrail-%r@%h:%p",
		"-o", "ControlPersist=60s",
		"-p", fmt.Sprintf("%d", s.port),
		s.host,
		cmd,
	}
}

func (s *SSH) Run(ctx context.Context, cmd string) (string, error) {
	return runCmd(exec.CommandContext(ctx, "ssh", s.sshArgs(cmd)...))
}
