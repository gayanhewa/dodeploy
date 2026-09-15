// Package remote runs commands and moves files on a host over ssh.
//
// Everything goes through one small type so ssh options, including the identity
// file, are applied consistently. Forgetting the identity on one code path is
// how "Permission denied (publickey)" appears for some commands and not others.
package remote

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Host is a machine reachable over ssh.
type Host struct {
	Address  string
	User     string
	Identity string
	// Timeout is applied by callers through the context, not here.
}

// Target is the user@address form.
func (h Host) Target() string {
	user := h.User
	if user == "" {
		user = "deploy"
	}
	return user + "@" + h.Address
}

// sshArgs are the options applied to every connection.
func (h Host) sshArgs() []string {
	args := []string{
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
	}
	if h.Identity != "" {
		args = append(args, "-i", expandHome(h.Identity))
	}
	return args
}

// Run executes a command, returning its combined output.
func (h Host) Run(ctx context.Context, command string) (string, error) {
	args := append(h.sshArgs(), h.Target(), command)
	cmd := exec.CommandContext(ctx, "ssh", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.String(), fmt.Errorf("ssh %s: %s: %w", h.Target(), truncate(msg, 400), err)
	}
	return stdout.String(), nil
}

// RunTolerant executes a command and reports only whether it succeeded, for
// probes where a non-zero exit is a normal answer.
func (h Host) RunTolerant(ctx context.Context, command string) bool {
	_, err := h.Run(ctx, command)
	return err == nil
}

// Output runs a command and returns its trimmed stdout.
func (h Host) Output(ctx context.Context, command string) (string, error) {
	out, err := h.Run(ctx, command)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// WriteFile places content at a remote path, optionally changing owner and mode
// through sudo.
//
// It writes to a temporary file first and uses install(1) to move it into place,
// so a file is never left half-written if the connection drops.
func (h Host) WriteFile(ctx context.Context, path, content string, mode os.FileMode, owner string) error {
	tmp, err := os.CreateTemp("", "dodeploy-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := h.Copy(ctx, tmp.Name(), "/tmp/dodeploy-upload"); err != nil {
		return err
	}

	install := fmt.Sprintf("install -m %04o", mode.Perm())
	if owner != "" {
		// install(1) takes the owner and group as separate flags; passing
		// "user:group" to -o fails with "invalid user".
		if user, group, ok := strings.Cut(owner, ":"); ok {
			install += " -o " + user + " -g " + group
		} else {
			install += " -o " + owner
		}
	}
	cmd := fmt.Sprintf("sudo %s /tmp/dodeploy-upload %s && rm -f /tmp/dodeploy-upload", install, shellQuote(path))
	if _, err := h.Run(ctx, cmd); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

// Copy transfers a local file to a remote path with scp.
func (h Host) Copy(ctx context.Context, local, remotePath string) error {
	args := append(h.sshArgs(), local, h.Target()+":"+remotePath)
	cmd := exec.CommandContext(ctx, "scp", args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scp to %s: %s: %w", remotePath, truncate(strings.TrimSpace(stderr.String()), 300), err)
	}
	return nil
}

// Sync mirrors a local directory to a remote one with rsync, deleting files that
// no longer exist locally.
//
// This is why application state lives outside the synced directory: anything
// under it is destroyed on the next deploy.
func (h Host) Sync(ctx context.Context, local, remoteDir string, excludes []string) error {
	sshCmd := "ssh"
	for _, a := range h.sshArgs() {
		sshCmd += " " + a
	}

	args := []string{"-az", "--delete", "-e", sshCmd}
	for _, e := range excludes {
		args = append(args, "--exclude", e)
	}
	if !strings.HasSuffix(local, "/") {
		local += "/"
	}
	args = append(args, local, h.Target()+":"+remoteDir)

	cmd := exec.CommandContext(ctx, "rsync", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync to %s: %s: %w", remoteDir, truncate(strings.TrimSpace(stderr.String()), 300), err)
	}
	return nil
}

// CheckTools verifies that ssh and rsync exist locally, so a failure names the
// missing tool rather than surfacing as a confusing connection error.
func CheckTools() error {
	for _, tool := range []string{"ssh", "rsync"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is required but was not found on PATH", tool)
		}
	}
	return nil
}

// WaitFor polls a command until it succeeds or the context expires.
func (h Host) WaitFor(ctx context.Context, command string, interval time.Duration) error {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if h.RunTolerant(ctx, command) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for: %s", command)
		case <-ticker.C:
		}
	}
}

// shellQuote wraps a path so spaces and globs cannot be interpreted.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
