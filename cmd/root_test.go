package cmd

import "testing"

func TestRootHasSubcommands(t *testing.T) {
	root := NewRootCmd()
	want := []string{"deploy", "rollback", "status", "logs", "check"}
	for _, name := range want {
		found := false
		for _, c := range root.Commands() {
			if c.Name() == name {
				found = true
			}
		}
		if !found {
			t.Errorf("missing subcommand %q", name)
		}
	}
}
