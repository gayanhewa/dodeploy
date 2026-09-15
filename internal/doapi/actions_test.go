package doapi

import "testing"

// The API's pagination links are absolute and include the version prefix, which
// the client also prepends. Getting this wrong asks for /v2/v2/... and every
// follow-up page 404s, which reads as "no more results" if the error is missed.
func TestNextPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{
			"https://api.digitalocean.com/v2/sizes?page=2&per_page=200",
			"/sizes?page=2&per_page=200",
		},
		{
			"https://api.digitalocean.com/v2/droplets?page=3",
			"/droplets?page=3",
		},
		{
			// A host we do not expect: fall back to the path so the request is
			// at least well formed rather than doubled.
			"https://example.test/v2/sizes?page=2",
			"/v2/sizes?page=2",
		},
	}
	for _, c := range cases {
		if got := nextPath(c.in); got != c.want {
			t.Errorf("nextPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
