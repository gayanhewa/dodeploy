// Package systemd renders the unit that runs an app.
package systemd

import (
	"fmt"
	"strings"
)

// Unit describes a service.
type Unit struct {
	Name             string
	User             string
	Group            string
	WorkingDirectory string
	EnvFile          string
	ExecStart        string
	// ReadWritePaths are the only directories the service may write to, which is
	// what lets ProtectSystem=strict be used at all.
	ReadWritePaths []string
}

// Render produces the unit file.
//
// The sandboxing is deliberate rather than decorative: these are small binaries
// with no need for a home directory, no need to see devices, and exactly one
// directory they write to. ProtectSystem=strict makes the rest of the filesystem
// read-only.
func Render(u Unit) string {
	if u.User == "" {
		u.User = "apps"
	}
	if u.Group == "" {
		u.Group = u.User
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", u.Name)
	fmt.Fprintf(&b, "After=network-online.target\n")
	fmt.Fprintf(&b, "Wants=network-online.target\n\n")

	fmt.Fprintf(&b, "[Service]\n")
	fmt.Fprintf(&b, "Type=simple\n")
	fmt.Fprintf(&b, "User=%s\n", u.User)
	fmt.Fprintf(&b, "Group=%s\n", u.Group)
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", u.WorkingDirectory)
	if u.EnvFile != "" {
		fmt.Fprintf(&b, "EnvironmentFile=%s\n", u.EnvFile)
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", u.ExecStart)
	fmt.Fprintf(&b, "Restart=always\n")
	fmt.Fprintf(&b, "RestartSec=3\n\n")

	fmt.Fprintf(&b, "NoNewPrivileges=true\n")
	fmt.Fprintf(&b, "PrivateTmp=true\n")
	fmt.Fprintf(&b, "PrivateDevices=true\n")
	fmt.Fprintf(&b, "ProtectSystem=strict\n")
	fmt.Fprintf(&b, "ProtectHome=true\n")
	fmt.Fprintf(&b, "ProtectKernelTunables=true\n")
	fmt.Fprintf(&b, "ProtectControlGroups=true\n")
	fmt.Fprintf(&b, "RestrictSUIDSGID=true\n")
	for _, p := range u.ReadWritePaths {
		if p != "" {
			fmt.Fprintf(&b, "ReadWritePaths=%s\n", p)
		}
	}
	fmt.Fprintf(&b, "CapabilityBoundingSet=\n\n")

	fmt.Fprintf(&b, "[Install]\n")
	fmt.Fprintf(&b, "WantedBy=multi-user.target\n")

	return b.String()
}
