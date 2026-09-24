// Package cloudflare talks to the Cloudflare API for the pieces dodeploy
// configures.
//
// Only Turnstile widgets are handled so far. A Turnstile widget is the right
// shape for this tool: it is declarative (a name, a mode, and the set of
// hostnames it accepts), it is reconciled rather than recreated, and it needs
// no per-visitor allow list the way a mail-domain filter would.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// apiBase is Cloudflare's API root, overridable on Client for tests.
const apiBase = "https://api.cloudflare.com/client/v4"

// defaultMode is used when a widget is first created and the spec does not name
// a mode.
const defaultMode = "managed"

// ErrDrift reports that the account does not match the desired configuration.
// A check maps it to a non-zero exit so it can gate a deploy; a normal run
// never returns it.
var ErrDrift = errors.New("cloudflare is out of date")

// Client is a minimal Cloudflare API client.
type Client struct {
	APIToken  string
	AccountID string
	HTTP      *http.Client
	// BaseURL overrides the API root in tests.
	BaseURL string
}

// New builds a client. The token needs Account -> Turnstile -> Edit.
func New(apiToken, accountID string) *Client {
	return &Client{
		APIToken:  apiToken,
		AccountID: accountID,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
	}
}

// Widget is a Turnstile widget.
//
// The read-only fields are carried so an update preserves settings dodeploy
// does not manage (bot fight mode, region, and so on) instead of resetting them
// to their zero values.
type Widget struct {
	SiteKey        string   `json:"sitekey,omitempty"`
	Secret         string   `json:"secret,omitempty"`
	Name           string   `json:"name"`
	Mode           string   `json:"mode,omitempty"`
	Domains        []string `json:"domains"`
	BotFightMode   bool     `json:"bot_fight_mode,omitempty"`
	Offlabel       bool     `json:"offlabel,omitempty"`
	EphemeralID    bool     `json:"ephemeral_id,omitempty"`
	Region         string   `json:"region,omitempty"`
	ClearanceLevel string   `json:"clearance_level,omitempty"`
}

// HasDomain reports whether the widget accepts tokens from hostname,
// case-insensitively and ignoring a trailing dot.
func (w Widget) HasDomain(hostname string) bool {
	for _, d := range w.Domains {
		if strings.EqualFold(normaliseDomain(d), normaliseDomain(hostname)) {
			return true
		}
	}
	return false
}

func normaliseDomain(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// Widgets lists every Turnstile widget in the account.
func (c *Client) Widgets(ctx context.Context) ([]Widget, error) {
	// Page until a short page arrives. do() returns only the result, not
	// result_info, so the page size is the signal that there is nothing more.
	const perPage = 50

	var out []Widget
	for page := 1; page <= 100; page++ {
		path := fmt.Sprintf("/accounts/%s/challenges/widgets?page=%d&per_page=%d", c.AccountID, page, perPage)
		raw, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}

		var widgets []Widget
		if err := json.Unmarshal(raw, &widgets); err != nil {
			return nil, fmt.Errorf("decode widgets: %w", err)
		}
		out = append(out, widgets...)
		if len(widgets) < perPage {
			break
		}
	}
	return out, nil
}

// WidgetByName finds a widget by its name, then by its sitekey, so a spec can
// name either.
func (c *Client) WidgetByName(ctx context.Context, nameOrKey string) (*Widget, error) {
	widgets, err := c.Widgets(ctx)
	if err != nil {
		return nil, err
	}
	for i := range widgets {
		if widgets[i].SiteKey == nameOrKey {
			return &widgets[i], nil
		}
	}
	for i := range widgets {
		if strings.EqualFold(widgets[i].Name, nameOrKey) {
			return &widgets[i], nil
		}
	}
	return nil, nil
}

// WidgetByKey fetches one widget, including its secret.
func (c *Client) WidgetByKey(ctx context.Context, siteKey string) (*Widget, error) {
	raw, err := c.do(ctx, http.MethodGet, "/accounts/"+c.AccountID+"/challenges/widgets/"+siteKey, nil)
	if err != nil {
		return nil, err
	}
	var w Widget
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("decode widget: %w", err)
	}
	return &w, nil
}

// CreateWidget adds a widget.
func (c *Client) CreateWidget(ctx context.Context, w Widget) (*Widget, error) {
	body := w
	body.SiteKey = ""
	body.Secret = ""
	raw, err := c.do(ctx, http.MethodPost, "/accounts/"+c.AccountID+"/challenges/widgets", body)
	if err != nil {
		return nil, err
	}
	return decodeWidget(raw)
}

// UpdateWidget replaces a widget's mutable settings, preserving the rest.
func (c *Client) UpdateWidget(ctx context.Context, siteKey string, w Widget) (*Widget, error) {
	body := w
	body.SiteKey = ""
	body.Secret = ""
	raw, err := c.do(ctx, http.MethodPut, "/accounts/"+c.AccountID+"/challenges/widgets/"+siteKey, body)
	if err != nil {
		return nil, err
	}
	return decodeWidget(raw)
}

// RotateSecret issues a new secret for a widget. The old secret keeps working
// until invalidateNow is set, which is what makes rotation safe to do before a
// restart.
func (c *Client) RotateSecret(ctx context.Context, siteKey string, invalidateNow bool) (*Widget, error) {
	body := map[string]bool{"invalidate_immediately": invalidateNow}
	raw, err := c.do(ctx, http.MethodPost, "/accounts/"+c.AccountID+"/challenges/widgets/"+siteKey+"/rotate_secret", body)
	if err != nil {
		return nil, err
	}
	return decodeWidget(raw)
}

func decodeWidget(raw json.RawMessage) (*Widget, error) {
	var w Widget
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("decode widget: %w", err)
	}
	return &w, nil
}

// EnsureOptions controls reconciliation.
type EnsureOptions struct {
	// DryRun reports what would change without calling any write endpoint.
	DryRun bool
	// Prune makes the domain set exact, removing hostnames another app may rely
	// on. Off by default because a widget is easily shared.
	Prune bool
}

// Result is what reconciling one widget changed (or would change).
type Result struct {
	Widget    Widget
	Created   bool
	Updated   bool
	Unchanged bool
}

// Changed reports whether the account needed a write.
func (r Result) Changed() bool { return r.Created || r.Updated }

// EnsureWidget reconciles the widget named in want to the desired domains and
// mode.
//
// It is additive about domains by default: a widget that several apps share
// keeps the hostnames the others added. A new widget is created when none has
// the name, and an existing one is updated in place so its secret is not
// disturbed.
func (c *Client) EnsureWidget(ctx context.Context, want Widget, opts EnsureOptions) (Result, error) {
	existing, err := c.WidgetByName(ctx, want.Name)
	if err != nil {
		return Result{}, err
	}

	if existing == nil {
		if want.Mode == "" {
			want.Mode = defaultMode
		}
		res := Result{Created: true, Widget: want}
		if opts.DryRun {
			return res, nil
		}
		created, err := c.CreateWidget(ctx, want)
		if err != nil {
			return Result{}, err
		}
		res.Widget = *created
		return res, nil
	}

	desired := *existing
	desired.Name = want.Name
	// An empty mode leaves the existing one alone, so sharing a widget does not
	// silently change its mode.
	if want.Mode != "" {
		desired.Mode = want.Mode
	}
	if opts.Prune {
		desired.Domains = want.Domains
	} else {
		desired.Domains = unionDomains(existing.Domains, want.Domains)
	}

	if domainsEqual(existing.Domains, desired.Domains) && existing.Mode == desired.Mode && existing.Name == desired.Name {
		// Still fetch the secret: it is needed for the .env even when nothing
		// about the widget changed.
		full := existing
		if full.Secret == "" && existing.SiteKey != "" {
			if got, err := c.WidgetByKey(ctx, existing.SiteKey); err == nil {
				full = got
			}
		}
		return Result{Unchanged: true, Widget: *full}, nil
	}

	res := Result{Updated: true, Widget: desired}
	if opts.DryRun {
		return res, nil
	}
	updated, err := c.UpdateWidget(ctx, existing.SiteKey, desired)
	if err != nil {
		return Result{}, err
	}
	res.Widget = *updated
	if res.Widget.Secret == "" {
		if got, err := c.WidgetByKey(ctx, existing.SiteKey); err == nil {
			res.Widget = *got
		}
	}
	return res, nil
}

// unionDomains returns a's domains followed by b's not already present.
func unionDomains(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, d := range list {
			key := normaliseDomain(d)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, d)
		}
	}
	return out
}

// domainsEqual compares two hostname lists as sets.
func domainsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, d := range a {
		seen[normaliseDomain(d)]++
	}
	for _, d := range b {
		k := normaliseDomain(d)
		seen[k]--
		if seen[k] < 0 {
			return false
		}
	}
	return true
}

// apiEnvelope is Cloudflare's standard response shape.
type apiEnvelope struct {
	Success  bool            `json:"success"`
	Errors   []apiError      `json:"errors"`
	Messages []apiError      `json:"messages"`
	Result   json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e apiError) String() string {
	if e.Message == "" {
		return fmt.Sprintf("code %d", e.Code)
	}
	return fmt.Sprintf("%s (code %d)", e.Message, e.Code)
}

// do performs one API call and returns the raw result.
func (c *Client) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	base := c.BaseURL
	if base == "" {
		base = apiBase
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudflare request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cloudflare returned %d and unreadable body: %w", resp.StatusCode, err)
	}
	if !env.Success {
		return nil, apiFailure(resp.StatusCode, env)
	}
	return env.Result, nil
}

// apiFailure turns an error envelope into something actionable, naming the fix
// for the two failures a misconfigured token actually produces.
func apiFailure(status int, env apiEnvelope) error {
	msgs := make([]string, 0, len(env.Errors))
	for _, e := range env.Errors {
		msgs = append(msgs, e.String())
	}
	detail := strings.Join(msgs, "; ")

	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("cloudflare rejected the API token (401%s): check providers.cloudflare.api_token", suffix(detail))
	case http.StatusForbidden:
		return fmt.Errorf("cloudflare denied the request (403%s): the token needs Account -> Turnstile -> Edit", suffix(detail))
	}
	return fmt.Errorf("cloudflare API error (%d%s)", status, suffix(detail))
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// SortedDomains returns the widget's domains in a stable order for output.
func SortedDomains(domains []string) []string {
	out := append([]string(nil), domains...)
	sort.Slice(out, func(i, j int) bool { return normaliseDomain(out[i]) < normaliseDomain(out[j]) })
	return out
}
