// Package cloudinit provides the first-boot configuration for a host.
package cloudinit

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed base.yaml
var base string

// Base returns the cloud-init user data.
//
// It refuses to return content containing a non-ASCII byte. cloud-init discards
// its ENTIRE configuration in that case, and does so silently: the host boots
// clean with nothing installed, no users created and no error logged, which
// presents as "the provisioning script did nothing". A single em-dash in a
// comment cost a full provision cycle before this check existed.
func Base() (string, error) {
	if line, b := firstNonASCII(base); line > 0 {
		return "", fmt.Errorf(
			"cloud-init template has a non-ASCII byte at line %d (0x%02x); "+
				"cloud-init would discard the whole configuration", line, b)
	}
	return base, nil
}

// firstNonASCII returns the 1-based line number and byte value of the first
// non-ASCII byte, or 0 when the text is plain ASCII.
func firstNonASCII(s string) (int, byte) {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			return strings.Count(s[:i], "\n") + 1, s[i]
		}
	}
	return 0, 0
}
