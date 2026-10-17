// Package mount attaches cluster storage locally with sshfs.
package mount

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/lunar-photon/cluster/internal/remote"
)

// IsMounted reports whether path is a mount point. It compares paths
// lexically and never stat()s the mount point: on a stale sshfs mount stat
// can block indefinitely, and the TUI calls this on a timer.
func IsMounted(path string) bool {
	target := normalize(path)
	if runtime.GOOS == "linux" {
		f, err := os.Open("/proc/mounts")
		if err != nil {
			return false
		}
		defer f.Close()
		return containsTarget(f, target)
	}
	// macOS/BSD: `mount` lists "dev on /path (type, ...)"
	out, err := exec.Command("mount").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if _, rest, ok := strings.Cut(line, " on "); ok {
			if i := strings.LastIndex(rest, " ("); i >= 0 && normalize(rest[:i]) == target {
				return true
			}
		}
	}
	return false
}

func normalize(p string) string {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	return p
}

func containsTarget(r io.Reader, target string) bool {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && unescape(fields[1]) == target {
			return true
		}
	}
	return false
}

// unescape decodes the \ooo octal escapes /proc/mounts uses for space, tab,
// newline and backslash.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Mount mounts host:remoteDir at local. It is a no-op if already mounted.
// It returns true if this call created the mount.
func Mount(r *remote.Client, remoteDir, local string) (bool, error) {
	if _, err := exec.LookPath("sshfs"); err != nil && !r.DryRun {
		return false, fmt.Errorf("sshfs is not installed (e.g. `sudo pacman -S sshfs` or `sudo apt install sshfs`)")
	}
	if IsMounted(local) {
		return false, nil
	}
	if !r.DryRun {
		if err := os.MkdirAll(local, 0o755); err != nil {
			return false, err
		}
	}
	opts := "reconnect,ServerAliveInterval=15,ServerAliveCountMax=3"
	if runtime.GOOS == "linux" {
		opts += ",idmap=user"
	}
	args := []string{
		r.Host + ":" + remoteDir, local,
		"-o", opts,
		"-o", "ssh_command=" + r.SSHCommandLine(),
	}
	if err := r.Local("sshfs", args...); err != nil {
		return false, fmt.Errorf("sshfs: %w", err)
	}
	if !r.DryRun && !IsMounted(local) {
		return false, fmt.Errorf("sshfs exited but %s is not mounted", local)
	}
	return true, nil
}

// Unmount detaches local. On Linux it uses a lazy unmount so a busy file
// manager window does not block it.
func Unmount(r *remote.Client, local string) error {
	if !r.DryRun && !IsMounted(local) {
		return nil
	}
	var candidates [][]string
	if runtime.GOOS == "linux" {
		candidates = [][]string{
			{"fusermount3", "-u", "-z", local},
			{"fusermount", "-u", "-z", local},
		}
	} else {
		candidates = [][]string{{"umount", local}, {"diskutil", "unmount", "force", local}}
	}
	var lastErr error
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		if lastErr = r.Local(c[0], c[1:]...); lastErr == nil {
			break
		}
	}
	if r.DryRun {
		return nil
	}
	if IsMounted(local) {
		if lastErr == nil {
			lastErr = fmt.Errorf("no unmount tool found")
		}
		return fmt.Errorf("could not unmount %s: %w", local, lastErr)
	}
	return nil
}

// Browse opens dir in the desktop file manager, in the background.
func Browse(dir string) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, dir)
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}
