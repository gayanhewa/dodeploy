package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPI is a minimal stand-in for the Turnstile endpoints. The list response
// deliberately omits secrets, as the real API does, so the code under test has
// to fetch one when it needs it.
type fakeAPI struct {
	widgets []Widget
	next    int
	calls   []string
}

const fakeAccount = "acct"

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)

	const base = "/accounts/" + fakeAccount + "/challenges/widgets"
	if !strings.HasPrefix(r.URL.Path, base) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, base)

	switch {
	case r.Method == http.MethodGet && rest == "":
		list := make([]Widget, 0, len(f.widgets))
		for _, x := range f.widgets {
			x.Secret = ""
			list = append(list, x)
		}
		writeResult(w, http.StatusOK, list)

	case r.Method == http.MethodPost && rest == "":
		var in Widget
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.next++
		in.SiteKey = fmt.Sprintf("0xKEY%d", f.next)
		in.Secret = fmt.Sprintf("secret-%d", f.next)
		f.widgets = append(f.widgets, in)
		writeResult(w, http.StatusOK, in)

	case r.Method == http.MethodGet && rest != "":
		if x, ok := f.find(strings.TrimPrefix(rest, "/")); ok {
			writeResult(w, http.StatusOK, x)
			return
		}
		writeResult(w, http.StatusNotFound, nil)

	case r.Method == http.MethodPut && rest != "":
		key := strings.TrimPrefix(rest, "/")
		var in Widget
		_ = json.NewDecoder(r.Body).Decode(&in)
		for i := range f.widgets {
			if f.widgets[i].SiteKey == key {
				in.SiteKey = key
				in.Secret = f.widgets[i].Secret
				f.widgets[i] = in
				writeResult(w, http.StatusOK, in)
				return
			}
		}
		writeResult(w, http.StatusNotFound, nil)

	case r.Method == http.MethodPost && strings.HasSuffix(rest, "/rotate_secret"):
		key := strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/rotate_secret")
		for i := range f.widgets {
			if f.widgets[i].SiteKey == key {
				f.widgets[i].Secret = "rotated-secret"
				writeResult(w, http.StatusOK, f.widgets[i])
				return
			}
		}
		writeResult(w, http.StatusNotFound, nil)

	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) find(key string) (Widget, bool) {
	for _, x := range f.widgets {
		if x.SiteKey == key {
			return x, true
		}
	}
	return Widget{}, false
}

func (f *fakeAPI) called(method, suffix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, method+" ") && strings.HasSuffix(c, suffix) {
			return true
		}
	}
	return false
}

func writeResult(w http.ResponseWriter, status int, result any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": status < 400,
		"result":  result,
		"errors":  []any{},
	})
}

func newTestClient(t *testing.T, f *fakeAPI) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New("token", fakeAccount)
	c.BaseURL = srv.URL
	return c
}

func TestEnsureWidgetCreates(t *testing.T) {
	f := &fakeAPI{}
	c := newTestClient(t, f)

	res, err := c.EnsureWidget(context.Background(), Widget{
		Name: "app", Mode: "managed", Domains: []string{"app.com"},
	}, EnsureOptions{})
	if err != nil {
		t.Fatalf("EnsureWidget: %v", err)
	}
	if !res.Created || res.Updated || res.Unchanged {
		t.Fatalf("flags = created:%v updated:%v unchanged:%v, want created only", res.Created, res.Updated, res.Unchanged)
	}
	if res.Widget.SiteKey == "" || res.Widget.Secret == "" {
		t.Fatalf("expected a site key and secret, got %+v", res.Widget)
	}
	if !f.called(http.MethodPost, "/challenges/widgets") {
		t.Fatal("expected a create call")
	}
}

// A new widget with no mode in the spec defaults to managed.
func TestEnsureWidgetDefaultsModeOnCreate(t *testing.T) {
	f := &fakeAPI{}
	c := newTestClient(t, f)

	res, err := c.EnsureWidget(context.Background(), Widget{
		Name: "app", Domains: []string{"app.com"},
	}, EnsureOptions{})
	if err != nil {
		t.Fatalf("EnsureWidget: %v", err)
	}
	if res.Widget.Mode != "managed" {
		t.Fatalf("mode = %q, want managed", res.Widget.Mode)
	}
}

// An app that names no mode must not change the mode of a widget it shares.
func TestEnsureWidgetEmptyModeLeavesExisting(t *testing.T) {
	f := &fakeAPI{widgets: []Widget{{
		SiteKey: "0xKEY1", Name: "shared", Mode: "invisible",
		Domains: []string{"a.com"}, Secret: "s",
	}}}
	c := newTestClient(t, f)

	res, err := c.EnsureWidget(context.Background(), Widget{
		Name: "shared", Domains: []string{"a.com"},
	}, EnsureOptions{})
	if err != nil {
		t.Fatalf("EnsureWidget: %v", err)
	}
	if !res.Unchanged {
		t.Fatalf("expected unchanged, got %+v", res)
	}
	if res.Widget.Mode != "invisible" {
		t.Fatalf("mode = %q, want the existing mode preserved", res.Widget.Mode)
	}
}

func TestEnsureWidgetUnchangedFetchesSecret(t *testing.T) {
	f := &fakeAPI{widgets: []Widget{{
		SiteKey: "0xKEY1", Name: "app", Mode: "managed",
		Domains: []string{"app.com"}, Secret: "existing-secret",
	}}}
	c := newTestClient(t, f)

	res, err := c.EnsureWidget(context.Background(), Widget{
		Name: "app", Mode: "managed", Domains: []string{"app.com"},
	}, EnsureOptions{})
	if err != nil {
		t.Fatalf("EnsureWidget: %v", err)
	}
	if !res.Unchanged {
		t.Fatalf("expected unchanged, got %+v", res)
	}
	if res.Widget.Secret != "existing-secret" {
		t.Fatalf("secret = %q, want it fetched from the widget", res.Widget.Secret)
	}
}

// A widget shared by several apps keeps the other apps' hostnames.
func TestEnsureWidgetAddsDomainsWithoutPruning(t *testing.T) {
	f := &fakeAPI{widgets: []Widget{{
		SiteKey: "0xKEY1", Name: "shared", Mode: "managed",
		Domains: []string{"a.com"}, Secret: "s",
	}}}
	c := newTestClient(t, f)

	res, err := c.EnsureWidget(context.Background(), Widget{
		Name: "shared", Mode: "managed", Domains: []string{"b.com"},
	}, EnsureOptions{})
	if err != nil {
		t.Fatalf("EnsureWidget: %v", err)
	}
	if !res.Updated {
		t.Fatalf("expected updated, got %+v", res)
	}
	if !res.Widget.HasDomain("a.com") || !res.Widget.HasDomain("b.com") {
		t.Fatalf("domains = %v, want both a.com and b.com", res.Widget.Domains)
	}
}

func TestEnsureWidgetPruneMakesDomainsExact(t *testing.T) {
	f := &fakeAPI{widgets: []Widget{{
		SiteKey: "0xKEY1", Name: "app", Mode: "managed",
		Domains: []string{"a.com"}, Secret: "s",
	}}}
	c := newTestClient(t, f)

	res, err := c.EnsureWidget(context.Background(), Widget{
		Name: "app", Mode: "managed", Domains: []string{"b.com"},
	}, EnsureOptions{Prune: true})
	if err != nil {
		t.Fatalf("EnsureWidget: %v", err)
	}
	if res.Widget.HasDomain("a.com") {
		t.Fatalf("domains = %v, want a.com pruned", res.Widget.Domains)
	}
	if !res.Widget.HasDomain("b.com") {
		t.Fatalf("domains = %v, want b.com present", res.Widget.Domains)
	}
}

// A check must not create anything.
func TestEnsureWidgetDryRunWritesNothing(t *testing.T) {
	f := &fakeAPI{}
	c := newTestClient(t, f)

	res, err := c.EnsureWidget(context.Background(), Widget{
		Name: "app", Mode: "managed", Domains: []string{"app.com"},
	}, EnsureOptions{DryRun: true})
	if err != nil {
		t.Fatalf("EnsureWidget: %v", err)
	}
	if !res.Created {
		t.Fatalf("expected created=true (would create), got %+v", res)
	}
	if f.called(http.MethodPost, "/challenges/widgets") {
		t.Fatal("dry run must not create a widget")
	}
}

func TestWidgetByNameMatchesSiteKeyAndName(t *testing.T) {
	f := &fakeAPI{widgets: []Widget{{SiteKey: "0xKEY1", Name: "Cram Sandwich", Domains: []string{"a.com"}}}}
	c := newTestClient(t, f)

	if _, err := c.WidgetByName(context.Background(), "0xKEY1"); err != nil {
		t.Fatalf("by sitekey: %v", err)
	}
	got, err := c.WidgetByName(context.Background(), "cram sandwich")
	if err != nil {
		t.Fatalf("by name: %v", err)
	}
	if got == nil {
		t.Fatal("expected a case-insensitive name match")
	}
}

func TestRotateSecret(t *testing.T) {
	f := &fakeAPI{widgets: []Widget{{SiteKey: "0xKEY1", Name: "app", Domains: []string{"a.com"}, Secret: "old"}}}
	c := newTestClient(t, f)

	rotated, err := c.RotateSecret(context.Background(), "0xKEY1", false)
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	if rotated.Secret != "rotated-secret" {
		t.Fatalf("secret = %q, want rotated-secret", rotated.Secret)
	}
}

// A token missing the Turnstile permission is the likely 403, so the message
// should name it.
func TestForbiddenNamesThePermission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": 10000, "message": "Authentication error"}},
		})
	}))
	defer srv.Close()

	c := New("token", fakeAccount)
	c.BaseURL = srv.URL

	_, err := c.Widgets(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Turnstile") {
		t.Fatalf("error %q should name the Turnstile permission", err)
	}
}

func TestDomainsEqualAndUnion(t *testing.T) {
	if !domainsEqual([]string{"A.com", "b.com."}, []string{"a.com.", "B.com"}) {
		t.Fatal("domains should compare case-insensitively and ignoring a trailing dot")
	}
	if domainsEqual([]string{"a.com"}, []string{"a.com", "b.com"}) {
		t.Fatal("lists of different sizes are not equal")
	}

	got := unionDomains([]string{"a.com"}, []string{"A.com", "b.com"})
	if len(got) != 2 || got[1] != "b.com" {
		t.Fatalf("union = %v, want [a.com b.com]", got)
	}
}
