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
	// Docker, when set, makes the unit run a container instead of a binary.
	Docker *DockerRun
}

// DockerRun describes a container app.
//
// systemd stays the supervisor, so status, logs and restart behaviour are the
// same as for a binary app; docker only runs the process.
type DockerRun struct {
	Image string
	// Name is the container name, used to clear a stale container before start.
	Name string
	// Publish is the port mapping, always host-loopback to container:
	// "127.0.0.1:3001:8080".
	Publish string
	// EnvFile is passed to docker run; Env entries override it.
	EnvFile string
	// Env are individual KEY=VALUE overrides, applied after EnvFile.
	Env []string
	// Volumes are bind mounts in "<host-path>:<container-path>" form.
	Volumes []string
}

// Render produces the unit file.
//
// The sandboxing is deliberate rather than decorative: these are small binaries
// with no need for a home directory, no need to see devices, and exactly one
// directory they write to. ProtectSystem=strict makes the rest of the filesystem
// read-only.
func Render(u Unit) string {
	if u.Docker != nil {
		return renderDocker(u)
	}
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

// renderDocker renders the unit for a container app.
//
// It deliberately omits the sandboxing the binary unit uses: the docker client
// has to reach the daemon socket, which is root-equivalent anyway, and
// ProtectSystem=strict would cut that socket off. For a container app the
// isolation comes from the container rather than from systemd.
func renderDocker(u Unit) string {
	d := u.Docker

	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", u.Name)
	fmt.Fprintf(&b, "After=network-online.target docker.service\n")
	fmt.Fprintf(&b, "Wants=network-online.target\n")
	fmt.Fprintf(&b, "Requires=docker.service\n\n")

	fmt.Fprintf(&b, "[Service]\n")
	fmt.Fprintf(&b, "Type=simple\n")
	// A failed start can leave the container behind, and docker run then dies on
	// the name conflict. The leading '-' tolerates "no such container".
	fmt.Fprintf(&b, "ExecStartPre=-/usr/bin/docker rm -f %s\n", d.Name)
	fmt.Fprintf(&b, "ExecStart=/usr/bin/docker run --rm --name %s --publish %s", d.Name, d.Publish)
	if d.EnvFile != "" {
		fmt.Fprintf(&b, " --env-file %s", d.EnvFile)
	}
	for _, e := range d.Env {
		fmt.Fprintf(&b, " --env %s", e)
	}
	for _, v := range d.Volumes {
		fmt.Fprintf(&b, " --volume %s", v)
	}
	fmt.Fprintf(&b, " %s\n", d.Image)
	fmt.Fprintf(&b, "Restart=always\n")
	fmt.Fprintf(&b, "RestartSec=3\n\n")

	fmt.Fprintf(&b, "[Install]\n")
	fmt.Fprintf(&b, "WantedBy=multi-user.target\n")

	return b.String()
}
