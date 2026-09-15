package host

import (
	"strings"
	"testing"

	"github.com/gayanhewa/dodeploy/internal/doapi"
)

// resizeHost builds the minimum Host that validateResize reads: its name, and
// the droplet's region and current size.
func resizeHost(currentSlug, region string) *Host {
	d := &doapi.Droplet{Name: "test-host", SizeSlug: currentSlug}
	d.Region.Slug = region
	return &Host{Name: "test-host", Droplet: d}
}

func size(slug string, memory, disk int, price float64, regions ...string) *doapi.Size {
	return &doapi.Size{
		Slug: slug, Memory: memory, Disk: disk,
		PriceMonthly: price, Regions: regions, Available: true,
	}
}

func TestValidateResize(t *testing.T) {
	current := size("s-1vcpu-512mb-10gb", 512, 10, 4, "syd1")

	cases := []struct {
		name    string
		target  *doapi.Size
		opts    ResizeOptions
		wantErr string // substring, empty means it should pass
	}{
		{
			name:   "growing disk and memory is allowed",
			target: size("s-1vcpu-1gb", 1024, 25, 6, "syd1"),
			opts:   ResizeOptions{Disk: true},
		},
		{
			name:   "memory only, same disk, is allowed",
			target: size("s-1vcpu-1gb-same-disk", 1024, 10, 6, "syd1"),
			opts:   ResizeOptions{Disk: false},
		},
		{
			// A disk can grow but never shrink, so this can never be valid.
			name:    "smaller disk is refused",
			target:  size("smaller", 1024, 5, 6, "syd1"),
			opts:    ResizeOptions{Disk: true},
			wantErr: "never shrink",
		},
		{
			name:    "less memory is refused",
			target:  size("smaller-mem", 256, 20, 3, "syd1"),
			opts:    ResizeOptions{Disk: true},
			wantErr: "less memory",
		},
		{
			// Resizing without --disk keeps the current disk, so a size with a
			// different disk would silently do something other than asked.
			name:    "disk change without the flag is refused",
			target:  size("s-1vcpu-1gb", 1024, 25, 6, "syd1"),
			opts:    ResizeOptions{Disk: false},
			wantErr: "Pass --disk",
		},
		{
			name:    "size unavailable in the region is refused",
			target:  size("s-1vcpu-1gb", 1024, 25, 6, "nyc1"),
			opts:    ResizeOptions{Disk: true},
			wantErr: "not offered in syd1",
		},
		{
			name:    "resizing to the current size is refused",
			target:  size("s-1vcpu-512mb-10gb", 512, 10, 4, "syd1"),
			opts:    ResizeOptions{Disk: false},
			wantErr: "already",
		},
		{
			// Passing --disk must not make a no-op resize look valid.
			name:    "resizing to the current size with --disk is refused",
			target:  size("s-1vcpu-512mb-10gb", 512, 10, 4, "syd1"),
			opts:    ResizeOptions{Disk: true},
			wantErr: "already",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := resizeHost(current.Slug, "syd1")
			err := h.validateResize(current, tc.target, tc.opts)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected this to be allowed, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected an error containing %q, got none", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// An unavailable size must be rejected even though everything else about it
// looks fine, otherwise the API returns a bare 422 much later.
func TestValidateResizeRejectsUnavailable(t *testing.T) {
	current := size("s-1vcpu-512mb-10gb", 512, 10, 4, "syd1")
	target := size("s-1vcpu-1gb", 1024, 25, 6, "syd1")
	target.Available = false

	h := resizeHost(current.Slug, "syd1")
	if err := h.validateResize(current, target, ResizeOptions{Disk: true}); err == nil {
		t.Fatal("expected an unavailable size to be refused")
	}
}
