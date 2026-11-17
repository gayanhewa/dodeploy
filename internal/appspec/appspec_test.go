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

func TestEnvProductionOverridesEnvAndAppliesEveryDeploy(t *testing.T) {
	spec, err := writeSpec(t, baseSpec+`
env:
  LOG_LEVEL: debug
  SHARED: from-env
env_production:
  SHARED: from-production
  DATABASE_URL: postgres://prod
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !spec.ManagedEnv() {
		t.Fatal("env_production must make the .env managed")
	}

	env := spec.RenderEnv("https://my-app.com")
	for _, want := range []string{
		"APP_ENV=prod",
		"APP_BASE_URL=https://my-app.com",
		"LOG_LEVEL=debug",
		"SHARED=from-production",
		"DATABASE_URL=postgres://prod",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env content missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "SHARED=from-env") {
		t.Fatalf("env_production must override env:\n%s", env)
	}
}

func TestEnvFileIsManagedAndNormalised(t *testing.T) {
	spec, err := writeSpec(t, baseSpec+`
env_file: ./deploy/.env.production
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !spec.ManagedEnv() {
		t.Fatal("env_file must make the .env managed")
	}
	if spec.EnvFile != "deploy/.env.production" {
		t.Fatalf("env_file = %q, want a cleaned relative path", spec.EnvFile)
	}
}

func TestEnvFileMustStayInsideTheApp(t *testing.T) {
	for _, path := range []string{"/etc/passwd", "../secrets", "deploy/../../secrets"} {
		_, err := writeSpec(t, baseSpec+"env_file: "+path+"\n")
		if err == nil {
			t.Fatalf("env_file %q should have been rejected", path)
		}
		if !strings.Contains(err.Error(), "env_file") {
			t.Fatalf("error %q should name the offending field", err)
		}
	}
}

func TestEnvFileAndEnvProductionCannotBothBeSet(t *testing.T) {
	_, err := writeSpec(t, baseSpec+`
env_file: .env.production
env_production:
  LOG_LEVEL: info
`)
	if err == nil {
		t.Fatal("expected an error when both env sources are set")
	}
	if !strings.Contains(err.Error(), "env_file") || !strings.Contains(err.Error(), "env_production") {
		t.Fatalf("error %q should name both fields", err)
	}
}

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
