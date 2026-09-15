package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const porkbunAPI = "https://api.porkbun.com/api/json/v3"

// ErrNotOptedIn is returned when the domain has not had API access enabled.
// Porkbun requires this to be switched on per account, and it cannot be enabled
// through the API, so it is worth naming clearly rather than surfacing as a
// generic 400.
var ErrNotOptedIn = errors.New("this domain is not opted in to Porkbun API access: " +
	"log in at porkbun.com, open Account -> API Access, and enable it " +
	"(globally, or for this domain)")

// Porkbun is a DNS provider backed by the Porkbun API.
type Porkbun struct {
	APIKey    string
	SecretKey string
	HTTP      *http.Client
}

// NewPorkbun builds a Porkbun provider.
func NewPorkbun(apiKey, secretKey string) *Porkbun {
	return &Porkbun{
		APIKey:    apiKey,
		SecretKey: secretKey,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
	}
}

type porkbunRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     string `json:"ttl"`
	Prio    string `json:"prio"`
}

// Records lists the zone's records, with names normalised to fully-qualified
// form so callers never see Porkbun's mixed spelling.
func (p *Porkbun) Records(ctx context.Context, hostname string) ([]Record, error) {
	zone, _, err := Split(hostname)
	if err != nil {
		return nil, err
	}

	var out struct {
		Records []porkbunRecord `json:"records"`
	}
	if err := p.call(ctx, "dns/retrieve/"+zone, nil, &out); err != nil {
		return nil, err
	}

	records := make([]Record, 0, len(out.Records))
	for _, r := range out.Records {
		records = append(records, Record{
			ID:      r.ID,
			Type:    r.Type,
			Name:    normalise(r.Name),
			Content: r.Content,
			TTL:     r.TTL,
		})
	}
	return records, nil
}

// Create adds a record. The API takes the bare host, not the full name.
func (p *Porkbun) Create(ctx context.Context, hostname string, r Record) error {
	zone, sub, err := Split(hostname)
	if err != nil {
		return err
	}
	body := map[string]string{
		"name":    sub,
		"type":    r.Type,
		"content": r.Content,
		"ttl":     ttlOr(r.TTL),
	}
	return p.call(ctx, "dns/create/"+zone, body, nil)
}

// Update edits a record in place.
func (p *Porkbun) Update(ctx context.Context, hostname string, r Record) error {
	zone, sub, err := Split(hostname)
	if err != nil {
		return err
	}
	body := map[string]string{
		"name":    sub,
		"type":    r.Type,
		"content": r.Content,
		"ttl":     ttlOr(r.TTL),
	}
	return p.call(ctx, "dns/edit/"+zone+"/"+r.ID, body, nil)
}

// Delete removes a record.
func (p *Porkbun) Delete(ctx context.Context, hostname string, id string) error {
	zone, _, err := Split(hostname)
	if err != nil {
		return err
	}
	err = p.call(ctx, "dns/delete/"+zone+"/"+id, nil, nil)
	// A record that is already gone is not a failure: a run can legitimately
	// remove the same wildcard twice.
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}

func (p *Porkbun) call(ctx context.Context, path string, body map[string]string, out any) error {
	payload := map[string]string{
		"apikey":       p.APIKey,
		"secretapikey": p.SecretKey,
	}
	for k, v := range body {
		payload[k] = v
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, porkbunAPI+"/"+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dodeploy/1.0")

	resp, err := p.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	// Porkbun answers 400 with a JSON body explaining the problem, so the status
	// code alone is not enough to report anything useful.
	var envelope struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("call %s: status %d: %s", path, resp.StatusCode, truncate(string(respBody), 200))
	}

	if envelope.Status != "SUCCESS" {
		msg := envelope.Message
		if msg == "" {
			msg = fmt.Sprintf("status %d", resp.StatusCode)
		}
		if strings.Contains(strings.ToLower(msg), "not opted in") {
			return ErrNotOptedIn
		}
		return fmt.Errorf("porkbun %s: %s", path, msg)
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

func ttlOr(ttl string) string {
	if strings.TrimSpace(ttl) == "" {
		return "600"
	}
	return ttl
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
