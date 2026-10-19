// Package remote runs commands on the cluster over ssh.
//
// Every ssh, rsync and sshfs invocation shares one multiplexed connection
// (OpenSSH ControlMaster), so password or 2FA prompts happen once and later
// calls cost a few milliseconds instead of a full handshake.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// Quote single-quotes s for a POSIX shell.
func Quote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:,@+%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Join quotes each argument and joins them into one shell command line.
func Join(args ...string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = Quote(a)
	}
	return strings.Join(q, " ")
}

// ErrConnection is returned when ssh itself fails (exit status 255), as
// opposed to the remote command failing.
var ErrConnection = errors.New("ssh connection failed")

// Client runs commands on one host.
type Client struct {
	Host string
	// SSH is the ssh binary; CLUSTER_SSH overrides it (used by tests).
	SSH string
	// ControlPath is the multiplexing socket; empty disables multiplexing.
	ControlPath string
	// Persist is how long the shared connection outlives its last use.
	Persist time.Duration
	// DryRun prints commands instead of running them.
	DryRun bool
	// Log receives dry-run output and command traces.
	Log io.Writer
}

// New returns a client with a control socket under $XDG_RUNTIME_DIR (or the
// temp dir), keyed by ssh's %C hash so it is unique per user/host/port.
func New(host string) *Client {
	ssh := os.Getenv("CLUSTER_SSH")
	if ssh == "" {
		ssh = "ssh"
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("cluster-%d", os.Getuid()))
	} else {
		dir = filepath.Join(dir, "cluster")
	}
	_ = os.MkdirAll(dir, 0o700)
	return &Client{
		Host:        host,
		SSH:         ssh,
		ControlPath: filepath.Join(dir, "ssh-%C"),
		Persist:     10 * time.Minute,
		Log:         os.Stderr,
	}
}

// SSHOptions are the -o flags shared by every ssh-based tool.
func (c *Client) SSHOptions() []string {
	// LogLevel=ERROR hides per-connection notices (e.g. a server's key-exchange
	// warning) on every call; real failures and host-key mismatches are errors
	// and still show. `cluster setup` reports such notices once.
	opts := []string{"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR"}
	if c.ControlPath != "" {
		opts = append(opts,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+c.ControlPath,
			"-o", fmt.Sprintf("ControlPersist=%d", int(c.Persist.Seconds())),
		)
	}
	return opts
}

// SSHCommandLine is the ssh invocation as one string, for rsync -e and sshfs ssh_command.
func (c *Client) SSHCommandLine() string {
	return Join(append([]string{c.SSH}, c.SSHOptions()...)...)
}

func (c *Client) sshArgs(flags []string, remoteCmd string) []string {
	args := append([]string{}, c.SSHOptions()...)
	args = append(args, flags...)
	args = append(args, c.Host)
	if remoteCmd != "" {
		args = append(args, remoteCmd)
	}
	return args
}

func (c *Client) trace(name string, args []string) {
	if c.DryRun && c.Log != nil {
		fmt.Fprintf(c.Log, "[dry-run] %s\n", Join(append([]string{name}, args...)...))
	}
}

// MasterAlive reports whether a shared connection is already up.
func (c *Client) MasterAlive() bool {
	if c.ControlPath == "" || c.DryRun {
		return c.DryRun
	}
	cmd := exec.Command(c.SSH, "-o", "ControlPath="+c.ControlPath, "-O", "check", c.Host)
	return cmd.Run() == nil
}

// EnsureMaster opens the shared connection in the foreground if needed, so
// that any password or 2FA prompt reaches the user's terminal. Later
// non-interactive calls (BatchMode) then ride on it.
func (c *Client) EnsureMaster() error {
	if c.ControlPath == "" || c.MasterAlive() {
		return nil
	}
	args := c.sshArgs([]string{"-o", "ControlMaster=yes", "-f", "-N"}, "")
	cmd := exec.Command(c.SSH, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: could not connect to %s: %v", ErrConnection, c.Host, err)
	}
	return nil
}

// Output runs remoteCmd (a shell command line) and returns its stdout.
// stdin may be nil.
func (c *Client) Output(ctx context.Context, remoteCmd string, stdin io.Reader) (string, error) {
	args := c.sshArgs([]string{"-o", "BatchMode=yes", "-T"}, remoteCmd)
	c.trace(c.SSH, args)
	if c.DryRun {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, c.SSH, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), wrapErr(err, stderr.String())
}

// Run is Output with argv quoted for the remote shell.
func (c *Client) Run(ctx context.Context, argv ...string) (string, error) {
	return c.Output(ctx, Join(argv...), nil)
}

// Interactive runs remoteCmd with a pty attached to the local terminal.
func (c *Client) Interactive(remoteCmd string) error {
	return c.InteractiveUntil(remoteCmd, nil)
}

// InteractiveUntil is Interactive, but if stop delivers a signal first, the
// ssh process is sent SIGHUP (then killed after 5s) and ErrStopped returned.
// ssh does not reliably exit by itself when the terminal goes away.
func (c *Client) InteractiveUntil(remoteCmd string, stop <-chan os.Signal) error {
	tty := "-T" // e.g. `cluster exec ... | less`: no pty, and no ssh warning about it
	if term.IsTerminal(int(os.Stdin.Fd())) {
		tty = "-t"
	}
	args := c.sshArgs([]string{tty}, remoteCmd)
	c.trace(c.SSH, args)
	if c.DryRun {
		return nil
	}
	cmd := exec.Command(c.SSH, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return wrapErr(err, "")
	case <-stop:
		_ = cmd.Process.Signal(syscall.SIGHUP)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		return ErrStopped
	}
}

// ErrStopped is returned by InteractiveUntil when its stop channel fired.
var ErrStopped = errors.New("stopped")

// Command builds an ssh exec.Cmd for callers that manage the process
// themselves (e.g. long-running tunnels).
func (c *Client) Command(flags []string, remoteCmd string) *exec.Cmd {
	args := c.sshArgs(flags, remoteCmd)
	c.trace(c.SSH, args)
	return exec.Command(c.SSH, args...)
}

// Local runs a local tool (rsync, sshfs, ...) honoring DryRun, attached to the terminal.
func (c *Client) Local(name string, args ...string) error {
	c.trace(name, args)
	if c.DryRun {
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func wrapErr(err error, stderr string) error {
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(stderr)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
		if msg != "" {
			return fmt.Errorf("%w: %s", ErrConnection, msg)
		}
		return ErrConnection
	}
	if msg != "" {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}

// ExitCode extracts a process exit status from err, or -1.
func ExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if errors.Is(err, ErrConnection) {
		return 255
	}
	return -1
}

// Reachable reports whether the host's ssh port accepts TCP connections. It
// resolves the host through `ssh -G` so ~/.ssh/config aliases work. When the
// host is reached through a proxy, it cannot be probed directly and Reachable
// returns true, leaving the real test to ssh.
func (c *Client) Reachable(ctx context.Context) bool {
	out, err := exec.CommandContext(ctx, c.SSH, "-G", c.Host).Output()
	if err != nil {
		return true
	}
	host, port := c.Host, "22"
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "hostname":
			host = v
		case "port":
			port = v
		case "proxyjump", "proxycommand":
			if v != "" && v != "none" {
				return true
			}
		}
	}
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Tunnel returns an ssh process forwarding 127.0.0.1:localPort to
// target:targetPort through the host. It uses its own connection rather than
// the shared one, so the tunnel's lifetime is exactly the process's lifetime
// and a dead tunnel is noticed as soon as the process exits.
func (c *Client) Tunnel(localPort int, target string, targetPort int) *exec.Cmd {
	args := []string{
		"-N",
		"-o", "ControlPath=none",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=10",
		"-o", "LogLevel=ERROR",
		"-L", fmt.Sprintf("127.0.0.1:%d:%s:%d", localPort, target, targetPort),
		c.Host,
	}
	c.trace(c.SSH, args)
	return exec.Command(c.SSH, args...)
}

// Notices opens one fresh, non-multiplexed connection at the normal log level
// and returns what ssh printed besides errors (e.g. warnings from the server).
func (c *Client) Notices(ctx context.Context) []string {
	cmd := exec.CommandContext(ctx, c.SSH, "-o", "ControlPath=none", "-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10", c.Host, "true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run()
	var out []string
	for _, line := range strings.Split(stderr.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
