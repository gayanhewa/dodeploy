package main

import (
	"flag"
	"testing"
)

// Go's flag package stops at the first non-flag argument, so a flag written after
// a positional one is silently ignored. For a CLI that means the flag appears
// accepted and does nothing, which is the worst possible failure. These cases
// pin the behaviour that flags work in either position.
func TestParseInterspersed(t *testing.T) {
	newFS := func() (*flag.FlagSet, *bool, *string, *string) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		skip := fs.Bool("skip-build", false, "")
		name := fs.String("name", "", "")
		dir := fs.String("dir", "", "")
		return fs, skip, name, dir
	}

	t.Run("bool flag after the positional", func(t *testing.T) {
		fs, skip, _, _ := newFS()
		pos, err := parseInterspersed(fs, []string{".", "--skip-build"})
		if err != nil {
			t.Fatal(err)
		}
		if !*skip {
			t.Error("--skip-build after the path was ignored")
		}
		if len(pos) != 1 || pos[0] != "." {
			t.Errorf("positional = %v, want [.]", pos)
		}
	})

	t.Run("bool flag before the positional", func(t *testing.T) {
		fs, skip, _, _ := newFS()
		pos, err := parseInterspersed(fs, []string{"--skip-build", "."})
		if err != nil {
			t.Fatal(err)
		}
		if !*skip || len(pos) != 1 || pos[0] != "." {
			t.Errorf("skip=%v pos=%v", *skip, pos)
		}
	})

	t.Run("value flag after the positional keeps its value", func(t *testing.T) {
		fs, _, name, _ := newFS()
		pos, err := parseInterspersed(fs, []string{".", "--name", "my-app"})
		if err != nil {
			t.Fatal(err)
		}
		if *name != "my-app" {
			t.Errorf("name = %q, want my-app", *name)
		}
		if len(pos) != 1 || pos[0] != "." {
			t.Errorf("positional = %v, want [.]", pos)
		}
	})

	t.Run("several positionals with interleaved flags", func(t *testing.T) {
		fs, skip, _, dir := newFS()
		pos, err := parseInterspersed(fs, []string{"my-app", "my-app.com", "--dir", "/tmp/x", "--skip-build"})
		if err != nil {
			t.Fatal(err)
		}
		if !*skip || *dir != "/tmp/x" {
			t.Errorf("skip=%v dir=%q", *skip, *dir)
		}
		if len(pos) != 2 || pos[0] != "my-app" || pos[1] != "my-app.com" {
			t.Errorf("positional = %v", pos)
		}
	})

	t.Run("equals form", func(t *testing.T) {
		fs, _, name, _ := newFS()
		pos, err := parseInterspersed(fs, []string{".", "--name=other"})
		if err != nil {
			t.Fatal(err)
		}
		if *name != "other" || len(pos) != 1 {
			t.Errorf("name=%q pos=%v", *name, pos)
		}
	})

	t.Run("no flags at all", func(t *testing.T) {
		fs, _, _, _ := newFS()
		pos, err := parseInterspersed(fs, []string{"a", "b"})
		if err != nil {
			t.Fatal(err)
		}
		if len(pos) != 2 {
			t.Errorf("positional = %v, want [a b]", pos)
		}
	})
}
