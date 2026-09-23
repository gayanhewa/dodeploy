package appspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSpec creates an app directory with a deploy/app.yaml and loads it.
func writeSpec(t *testing.T, body string) (*Spec, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(dir)
}

const baseSpec = `
name: my-app
domain: my-app.com
port: 3001
build:
  package: ./cmd/server
  binary: server
`

func TestTurnstileNotEnabledByDefault(t *testing.T) {
	spec, err := writeSpec(t, baseSpec)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if spec.TurnstileEnabled() {
		t.Fatal("an app without a cloudflare block must not be enabled")
	}
}

func TestTurnstileDefaultsAndDomains(t *testing.T) {
	spec, err := writeSpec(t, baseSpec+`
aliases: [www.my-app.com]
cloudflare:
  turnstile:
    domains: [localhost]
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !spec.TurnstileEnabled() {
		t.Fatal("expected Turnstile to be enabled")
	}
	if got := spec.TurnstileMode(); got != "" {
		t.Fatalf("mode = %q, want empty so an existing widget's mode is left alone", got)
	}
	if got := spec.TurnstileWidgetName(); got != "my-app" {
		t.Fatalf("widget = %q, want the app name", got)
	}
	siteKey, secretKey := spec.TurnstileEnvNames()
	if siteKey != DefaultTurnstileSiteKeyEnv || secretKey != DefaultTurnstileSecretKeyEnv {
		t.Fatalf("env names = %q/%q, want the defaults", siteKey, secretKey)
	}

	domains := spec.TurnstileDomains()
	want := []string{"my-app.com", "www.my-app.com", "localhost"}
	if len(domains) != len(want) {
		t.Fatalf("domains = %v, want %v", domains, want)
	}
	for i := range want {
		if domains[i] != want[i] {
			t.Fatalf("domains = %v, want %v", domains, want)
		}
	}
}

func TestTurnstileSharedWidgetAndCustomEnv(t *testing.T) {
	spec, err := writeSpec(t, baseSpec+`
cloudflare:
  turnstile:
    widget: shared-widget
    env:
      site_key: MY_SITE_KEY
      secret_key: MY_SECRET_KEY
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := spec.TurnstileWidgetName(); got != "shared-widget" {
		t.Fatalf("widget = %q, want shared-widget", got)
	}
	siteKey, secretKey := spec.TurnstileEnvNames()
	if siteKey != "MY_SITE_KEY" || secretKey != "MY_SECRET_KEY" {
		t.Fatalf("env names = %q/%q, want the configured ones", siteKey, secretKey)
	}
}

func TestTurnstileModeIsPassedThroughWhenSet(t *testing.T) {
	spec, err := writeSpec(t, baseSpec+`
cloudflare:
  turnstile:
    mode: invisible
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := spec.TurnstileMode(); got != TurnstileModeInvisible {
		t.Fatalf("mode = %q, want %q", got, TurnstileModeInvisible)
	}
}

func TestTurnstileRejectsUnknownMode(t *testing.T) {
	_, err := writeSpec(t, baseSpec+`
cloudflare:
  turnstile:
    mode: impossible
`)
	if err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
	if !strings.Contains(err.Error(), "cloudflare.turnstile.mode") {
		t.Fatalf("error %q should name the offending field", err)
	}
}
