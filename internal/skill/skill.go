// Package skill installs the agent skill that documents dodeploy, so an AI
// coding assistant working in a repository knows how to deploy with the tool.
//
// The skill is embedded in the binary, so `dodeploy skills install` works from a
// single downloaded executable with no repository checkout.
package skill

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	dodeploy "github.com/gayanhewa/dodeploy"
)

// Name is the skill's directory name, both in the repository and once installed.
const Name = "dodeploy"

// source is the skill's directory in the embedded filesystem.
const source = "skills/" + Name

// Target is a place the skill can be installed for a coding assistant.
type Target struct {
	// Label names the convention being served, for output.
	Label string
	// Dir is the full skill directory; SKILL.md is written directly inside it.
	Dir string
}

// Targets returns the default install locations.
//
// Two conventions are covered because agent harnesses disagree: the vendor
// neutral .agents/skills, and Claude Code's .claude/skills. Installing into both
// costs a few kilobytes and means the skill is found whichever one is read. An
// empty base directory means the working directory.
func Targets(global bool) ([]Target, error) {
	base := "."
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("find home directory: %w", err)
		}
		base = home
	}
	return TargetsAt(base), nil
}

// TargetsAt returns the install locations under a base directory, such as a
// project root or a home directory.
func TargetsAt(base string) []Target {
	return []Target{
		{Label: "agents", Dir: filepath.Join(base, ".agents", "skills", Name)},
		{Label: "claude", Dir: filepath.Join(base, ".claude", "skills", Name)},
	}
}

// TargetsIn returns the skill directories for a list of skills directories, so
// `--dir ~/.agents/skills` installs to `~/.agents/skills/dodeploy`.
func TargetsIn(skillsDirs []string) []Target {
	var out []Target
	for _, d := range skillsDirs {
		out = append(out, Target{Label: filepath.Base(filepath.Clean(d)), Dir: filepath.Join(d, Name)})
	}
	return out
}

// Status describes what happened to one target.
type Status string

const (
	// Installed means the files were written.
	Installed Status = "installed"
	// UpToDate means the target already held identical content.
	UpToDate Status = "up to date"
	// Skipped means the target held different content and force was not set.
	Skipped Status = "skipped (exists with different content; use --force)"
)

// Installation reports the outcome for one target.
type Installation struct {
	Target Target
	Status Status
	Files  int
}

// Install writes the embedded skill into each target.
//
// An existing, identical installation is reported as up to date rather than
// rewritten, matching the idempotence of the rest of the tool. A target with
// different content is left alone unless force is set, so an unrelated skill of
// the same name is never clobbered by accident.
func Install(targets []Target, force bool) ([]Installation, error) {
	content, err := files()
	if err != nil {
		return nil, err
	}

	results := make([]Installation, 0, len(targets))
	for _, t := range targets {
		existing, err := os.ReadFile(filepath.Join(t.Dir, "SKILL.md"))
		switch {
		case err == nil && !force:
			if bytes.Equal(existing, content["SKILL.md"]) {
				results = append(results, Installation{Target: t, Status: UpToDate})
				continue
			}
			results = append(results, Installation{Target: t, Status: Skipped})
			continue
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return results, fmt.Errorf("read %s: %w", t.Dir, err)
		}

		if err := writeAll(t.Dir, content); err != nil {
			return results, err
		}
		results = append(results, Installation{Target: t, Status: Installed, Files: len(content)})
	}
	return results, nil
}

// Content returns the skill's files, keyed by their path relative to the skill
// directory. Slashes are forward slashes regardless of platform.
func Content() (map[string][]byte, error) { return files() }

// Body returns the SKILL.md contents.
func Body() ([]byte, error) {
	return dodeploy.Skills.ReadFile(path.Join(source, "SKILL.md"))
}

func files() (map[string][]byte, error) {
	out := map[string][]byte{}
	err := fs.WalkDir(dodeploy.Skills, source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := dodeploy.Skills.ReadFile(p)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(p, source+"/")] = body
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read embedded skill: %w", err)
	}
	return out, nil
}

func writeAll(dir string, content map[string][]byte) error {
	for name, body := range content {
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}
	return nil
}
