package caddy

import (
	"strings"
	"testing"
)

func TestRenderIncludesEverySiteAndAlias(t *testing.T) {
	got := Render("admin@example.com", []Site{
		{Domain: "example.com", Aliases: []string{"www.example.com"}, Port: 3001},
		{Domain: "other.test", Port: 3002},
	})

	for _, want := range []string{
		"email admin@example.com",
		"example.com {",
		"reverse_proxy 127.0.0.1:3001",
		"www.example.com {",
		"redir https://example.com{uri} permanent",
		"other.test {",
		"reverse_proxy 127.0.0.1:3002",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered config is missing %q\n\n%s", want, got)
		}
	}
}

// Stable output matters: the deploy compares the rendered result against the
// host and skips the reload when nothing changed.
func TestRenderIsStableRegardlessOfInputOrder(t *testing.T) {
	a := Render("x@y.z", []Site{
		{Domain: "b.test", Port: 3002},
		{Domain: "a.test", Port: 3001},
	})
	b := Render("x@y.z", []Site{
		{Domain: "a.test", Port: 3001},
		{Domain: "b.test", Port: 3002},
	})
	if a != b {
		t.Error("rendering should not depend on the order sites are supplied in")
	}
	if strings.Index(a, "a.test") > strings.Index(a, "b.test") {
		t.Error("sites should be sorted by domain")
	}
}

func TestWithWWW(t *testing.T) {
	if got := WithWWW("example.com", nil); len(got) != 1 || got[0] != "www.example.com" {
		t.Errorf("expected a www alias, got %v", got)
	}
	// Adding it twice must not duplicate.
	once := WithWWW("example.com", nil)
	twice := WithWWW("example.com", once)
	if len(twice) != 1 {
		t.Errorf("www alias duplicated: %v", twice)
	}
	// A www domain does not need a www alias of its own.
	if got := WithWWW("www.example.com", nil); len(got) != 0 {
		t.Errorf("expected no alias for a www domain, got %v", got)
	}
}
