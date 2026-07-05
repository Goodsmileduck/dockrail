package connection

import (
	"context"
	"strings"
)

type stub struct {
	substr string
	stdout string
	err    error
}

type Fake struct {
	Commands []string
	stubs    []stub
}

func NewFake() *Fake { return &Fake{} }

func (f *Fake) Stub(substr, stdout string, err error) {
	f.stubs = append(f.stubs, stub{substr, stdout, err})
}

func (f *Fake) Run(_ context.Context, cmd string) (string, error) {
	f.Commands = append(f.Commands, cmd)
	for _, s := range f.stubs {
		if strings.Contains(cmd, s.substr) {
			return s.stdout, s.err
		}
	}
	return "", nil
}
