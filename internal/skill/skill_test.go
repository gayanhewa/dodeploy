package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentHasFrontmatter(t *testing.T) {
	body, err := Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	text := string(body)
	for _, want := range []string{"---\nname: dodeploy\n", "description:"} {
		if !strings.Contains(text, want) {
			t.Errorf("SKILL.md missing %q", want)
		}
	}
}

func TestInstallWritesSkill(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	targets := TargetsIn([]string{dir})

	results, err := Install(targets, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(results) != 1 || results[0].Status != Installed {
		t.Fatalf("results = %+v, want one installed", results)
	}

	dst := filepath.Join(dir, Name, "SKILL.md")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read installed skill: %v", err)
	}
	want, _ := Body()
	if string(got) != string(want) {
		t.Error("installed SKILL.md differs from the embedded one")
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	targets := TargetsIn([]string{dir})

	if _, err := Install(targets, false); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	results, err := Install(targets, false)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if results[0].Status != UpToDate {
		t.Errorf("status = %q, want %q", results[0].Status, UpToDate)
	}
}

func TestInstallRefusesToClobberWithoutForce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	dst := filepath.Join(dir, Name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "SKILL.md"), []byte("someone else's skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	results, err := Install(TargetsIn([]string{dir}), false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if results[0].Status != Skipped {
		t.Fatalf("status = %q, want %q", results[0].Status, Skipped)
	}
	body, _ := os.ReadFile(filepath.Join(dst, "SKILL.md"))
	if string(body) != "someone else's skill" {
		t.Error("existing skill was overwritten without --force")
	}

	if _, err := Install(TargetsIn([]string{dir}), true); err != nil {
		t.Fatalf("force Install: %v", err)
	}
	if body, _ = os.ReadFile(filepath.Join(dst, "SKILL.md")); string(body) == "someone else's skill" {
		t.Error("force did not overwrite the skill")
	}
}

func TestTargetsAtUsesBothConventions(t *testing.T) {
	targets := TargetsAt("/base")
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}
	if targets[0].Dir != filepath.Join("/base", ".agents", "skills", Name) {
		t.Errorf("first target = %q", targets[0].Dir)
	}
	if targets[1].Dir != filepath.Join("/base", ".claude", "skills", Name) {
		t.Errorf("second target = %q", targets[1].Dir)
	}
}
