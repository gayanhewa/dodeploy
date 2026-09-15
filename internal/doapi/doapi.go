// Package doapi is a small DigitalOcean API client covering only what dodeploy
// needs: droplets, ssh keys and reserved IPs.
//
// It talks to the API directly rather than shelling out to doctl. Parsing
// another tool's human-readable output is what made the original scripts
// fragile, and typed responses remove that class of bug entirely.
package doapi

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

const baseURL = "https://api.digitalocean.com/v2"

// ErrNotFound is returned when a resource does not exist.
var ErrNotFound = errors.New("not found")

// Client is a DigitalOcean API client.
type Client struct {
	token string
	http  *http.Client
}

// New builds a client with the given API token.
func New(token string) *Client {
	return &Client{
		token: token,
		http:  &http.Client{Timeout: 60 * time.Second},
	}
}

// Droplet is a machine.
type Droplet struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Region struct {
		Slug string `json:"slug"`
	} `json:"region"`
	SizeSlug string `json:"size_slug"`
	Networks struct {
		V4 []struct {
			IPAddress string `json:"ip_address"`
			Type      string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
}

// PublicIPv4 returns the droplet's first public IPv4 address. With a reserved IP
// attached a droplet has two, and the anchor is listed first, so callers that
// need a stable address should prefer ReservedIP for the droplet instead.
func (d Droplet) PublicIPv4() string {
	for _, n := range d.Networks.V4 {
		if n.Type == "public" {
			return n.IPAddress
		}
	}
	return ""
}

// PublicIPv4s returns every public IPv4 address on the droplet.
func (d Droplet) PublicIPv4s() []string {
	var out []string
	for _, n := range d.Networks.V4 {
		if n.Type == "public" {
			out = append(out, n.IPAddress)
		}
	}
	return out
}

// SSHKey is an account-level public key.
type SSHKey struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}

// ReservedIP is a floating address that survives rebuilding a droplet.
type ReservedIP struct {
	IP      string   `json:"ip"`
	Droplet *Droplet `json:"droplet"`
	Region  struct {
		Slug string `json:"slug"`
	} `json:"region"`
}

// Droplets lists every droplet on the account.
func (c *Client) Droplets(ctx context.Context) ([]Droplet, error) {
	var out struct {
		Droplets []Droplet `json:"droplets"`
	}
	if err := c.do(ctx, http.MethodGet, "/droplets?per_page=200", nil, &out); err != nil {
		return nil, err
	}
	return out.Droplets, nil
}

// DropletByName finds a droplet by name, returning ErrNotFound when absent.
func (c *Client) DropletByName(ctx context.Context, name string) (*Droplet, error) {
	droplets, err := c.Droplets(ctx)
	if err != nil {
		return nil, err
	}
	for i := range droplets {
		if droplets[i].Name == name {
			return &droplets[i], nil
		}
	}
	return nil, fmt.Errorf("droplet %q: %w", name, ErrNotFound)
}

// CreateDropletRequest describes a droplet to create.
type CreateDropletRequest struct {
	Name       string   `json:"name"`
	Region     string   `json:"region"`
	Size       string   `json:"size"`
	Image      string   `json:"image"`
	SSHKeys    []string `json:"ssh_keys,omitempty"`
	UserData   string   `json:"user_data,omitempty"`
	IPv6       bool     `json:"ipv6"`
	Monitoring bool     `json:"monitoring"`
}

// CreateDroplet creates a droplet and returns it.
func (c *Client) CreateDroplet(ctx context.Context, req CreateDropletRequest) (*Droplet, error) {
	var out struct {
		Droplet Droplet `json:"droplet"`
	}
	if err := c.do(ctx, http.MethodPost, "/droplets", req, &out); err != nil {
		return nil, err
	}
	return &out.Droplet, nil
}

// DeleteDroplet destroys a droplet.
func (c *Client) DeleteDroplet(ctx context.Context, id int) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/droplets/%d", id), nil, nil)
}

// SSHKeys lists the account's keys.
func (c *Client) SSHKeys(ctx context.Context) ([]SSHKey, error) {
	var out struct {
		Keys []SSHKey `json:"ssh_keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/account/keys?per_page=200", nil, &out); err != nil {
		return nil, err
	}
	return out.Keys, nil
}

// ResolveSSHKeys turns names, ids or fingerprints into fingerprints, which is
// what the droplet API accepts. Passing a name directly fails with a bare 422.
func (c *Client) ResolveSSHKeys(ctx context.Context, refs []string) ([]string, error) {
	keys, err := c.SSHKeys(ctx)
	if err != nil {
		return nil, err
	}

	// Empty means every key on the account.
	if len(refs) == 0 {
		out := make([]string, 0, len(keys))
		for _, k := range keys {
			out = append(out, k.Fingerprint)
		}
		if len(out) == 0 {
			return nil, errors.New("no SSH keys on this DigitalOcean account; add one first")
		}
		return out, nil
	}

	var out []string
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		// A fingerprint or numeric id passes straight through.
		if strings.Contains(ref, ":") || isNumeric(ref) {
			out = append(out, ref)
			continue
		}
		var matched bool
		for _, k := range keys {
			if k.Name == ref {
				out = append(out, k.Fingerprint)
				matched = true
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("no SSH key named %q on this account", ref)
		}
	}
	return out, nil
}

// ReservedIPs lists reserved IPs.
func (c *Client) ReservedIPs(ctx context.Context) ([]ReservedIP, error) {
	var out struct {
		ReservedIPs []ReservedIP `json:"reserved_ips"`
	}
	if err := c.do(ctx, http.MethodGet, "/reserved_ips?per_page=200", nil, &out); err != nil {
		return nil, err
	}
	return out.ReservedIPs, nil
}

// ReservedIPForDroplet returns the reserved IP attached to a droplet, if any.
func (c *Client) ReservedIPForDroplet(ctx context.Context, dropletName string) (string, error) {
	ips, err := c.ReservedIPs(ctx)
	if err != nil {
		return "", err
	}
	for _, ip := range ips {
		if ip.Droplet != nil && ip.Droplet.Name == dropletName {
			return ip.IP, nil
		}
	}
	return "", nil
}

// CreateReservedIP reserves an address in a region.
func (c *Client) CreateReservedIP(ctx context.Context, region string) (*ReservedIP, error) {
	body := map[string]any{"region": region}
	var out struct {
		ReservedIP ReservedIP `json:"reserved_ip"`
	}
	if err := c.do(ctx, http.MethodPost, "/reserved_ips", body, &out); err != nil {
		return nil, err
	}
	return &out.ReservedIP, nil
}

// AssignReservedIP attaches a reserved IP to a droplet.
func (c *Client) AssignReservedIP(ctx context.Context, ip string, dropletID int) error {
	body := map[string]any{"type": "assign", "droplet_id": dropletID}
	return c.do(ctx, http.MethodPost, "/reserved_ips/"+ip+"/actions", body, nil)
}

// ---------------------------------------------------------------------------

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dodeploy/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	if resp.StatusCode >= 300 {
		// The API explains refusals in the body, and that explanation is far more
		// useful than the status code alone.
		var apiErr struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(respBody, &apiErr)
		if apiErr.Message != "" {
			return fmt.Errorf("%s %s: %s (status %d)", method, path, apiErr.Message, resp.StatusCode)
		}
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, truncate(string(respBody), 200))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
