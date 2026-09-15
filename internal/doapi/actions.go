package doapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Size is an available droplet size.
type Size struct {
	Slug         string   `json:"slug"`
	Memory       int      `json:"memory"` // MB
	VCPUs        int      `json:"vcpus"`
	Disk         int      `json:"disk"` // GB
	PriceMonthly float64  `json:"price_monthly"`
	Regions      []string `json:"regions"`
	Available    bool     `json:"available"`
	Description  string   `json:"description"`
}

// pagination is embedded in list responses so pagination can be followed. The
// sizes list runs to several hundred entries and is served 200 at a time.
type pagination struct {
	Links struct {
		Pages struct {
			Next string `json:"next"`
		} `json:"pages"`
	} `json:"links"`
}

// nextPath turns the absolute pagination URL the API returns into a path this
// client can request.
//
// The returned URL already includes the API's version prefix, and the client
// prepends its own base, so the prefix has to be stripped or every follow-up
// request asks for /v2/v2/... and comes back 404.
func nextPath(next string) string {
	if next == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(next, baseURL); ok {
		return rest
	}

	u, err := url.Parse(next)
	if err != nil {
		return ""
	}
	if u.RawQuery == "" {
		return u.Path
	}
	return u.Path + "?" + u.RawQuery
}

// Sizes lists every droplet size on offer.
//
// Every page is followed: the API returns 200 at a time and there are more than
// 300, so a single request silently omits the rest.
func (c *Client) Sizes(ctx context.Context) ([]Size, error) {
	var all []Size

	path := "/sizes?per_page=200"
	for path != "" {
		var out struct {
			Sizes []Size `json:"sizes"`
			pagination
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Sizes...)
		path = nextPath(out.Links.Pages.Next)
	}
	return all, nil
}

// SizeBySlug finds one size by its slug.
//
// The API's per-size endpoint only accepts the numeric id: asking for
// /v2/sizes/s-1vcpu-1gb answers 404 "could not be routed" rather than returning
// the size. The paginated list is searched instead.
func (c *Client) SizeBySlug(ctx context.Context, slug string) (*Size, error) {
	sizes, err := c.Sizes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range sizes {
		if sizes[i].Slug == slug {
			return &sizes[i], nil
		}
	}
	return nil, fmt.Errorf("size %q: %w", slug, ErrNotFound)
}

// Action is an asynchronous operation on a resource.
type Action struct {
	ID          int    `json:"id"`
	Status      string `json:"status"` // in-progress, completed, errored
	Type        string `json:"type"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
}

// Done reports whether the action has finished, successfully or otherwise.
func (a Action) Done() bool {
	return a.Status == "completed" || a.Status == "errored"
}

// Resize changes a droplet's size.
//
// disk controls whether the disk is grown as well. That is a one-way change:
// DigitalOcean can grow a disk but never shrink it, so a disk resize cannot be
// undone even by resizing back down.
//
// DigitalOcean requires the droplet to be powered off. Callers should expect to
// power it off first rather than relying on this succeeding while it runs.
func (c *Client) Resize(ctx context.Context, dropletID int, size string, disk bool) (*Action, error) {
	body := map[string]any{"type": "resize", "size": size, "disk": disk}
	return c.dropletAction(ctx, dropletID, body)
}

// PowerOff shuts a droplet down.
func (c *Client) PowerOff(ctx context.Context, dropletID int) (*Action, error) {
	return c.dropletAction(ctx, dropletID, map[string]any{"type": "power_off"})
}

// PowerOn starts a droplet.
func (c *Client) PowerOn(ctx context.Context, dropletID int) (*Action, error) {
	return c.dropletAction(ctx, dropletID, map[string]any{"type": "power_on"})
}

// Snapshot captures a droplet image, which is the only undo available for a disk
// resize.
func (c *Client) Snapshot(ctx context.Context, dropletID int, name string) (*Action, error) {
	return c.dropletAction(ctx, dropletID, map[string]any{"type": "snapshot", "name": name})
}

// Reboot restarts a droplet without powering it off.
func (c *Client) Reboot(ctx context.Context, dropletID int) (*Action, error) {
	return c.dropletAction(ctx, dropletID, map[string]any{"type": "reboot"})
}

func (c *Client) dropletAction(ctx context.Context, dropletID int, body map[string]any) (*Action, error) {
	var out struct {
		Action Action `json:"action"`
	}
	path := fmt.Sprintf("/droplets/%d/actions", dropletID)
	if err := c.do(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out.Action, nil
}

// ActionStatus reads an action's current state.
func (c *Client) ActionStatus(ctx context.Context, dropletID, actionID int) (*Action, error) {
	var out struct {
		Action Action `json:"action"`
	}
	path := fmt.Sprintf("/droplets/%d/actions/%d", dropletID, actionID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out.Action, nil
}

// WaitForAction polls until an action finishes, and reports a failure when it
// errors rather than leaving the caller to notice.
func (c *Client) WaitForAction(ctx context.Context, dropletID, actionID int, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)

	for {
		action, err := c.ActionStatus(ctx, dropletID, actionID)
		if err != nil {
			return err
		}
		switch action.Status {
		case "completed":
			return nil
		case "errored":
			return fmt.Errorf("digitalocean reported the %s action as errored", action.Type)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for the %s action after %s", action.Type, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// DropletStatus reads a droplet's current state, for polling after a power
// change.
func (c *Client) DropletStatus(ctx context.Context, dropletID int) (string, error) {
	var out struct {
		Droplet Droplet `json:"droplet"`
	}
	path := fmt.Sprintf("/droplets/%d", dropletID)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return "", err
	}
	return out.Droplet.Status, nil
}

// WaitForPowerState polls until the droplet reports the given status.
func (c *Client) WaitForPowerState(ctx context.Context, dropletID int, want string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)

	for {
		status, err := c.DropletStatus(ctx, dropletID)
		if err != nil {
			return err
		}
		if status == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("droplet still reports %q after %s, wanted %q", status, timeout, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// IsNotPoweredOff reports whether an API error looks like DigitalOcean refusing
// an operation because the droplet is still running. The wording is not part of
// the API contract, so callers should treat a false negative as possible and be
// prepared to power off anyway.
func IsNotPoweredOff(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"power off", "powered off", "must be off", "not powered off"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
