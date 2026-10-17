package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lunar-photon/cluster/internal/lifecycle"
	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/slurm"
)

// The tmux release `cluster setup` builds, pinned by checksum.
const (
	tmuxVersion = "3.8"
	tmuxSHA256  = "e79c699c7e949dccd0a4a125e17b8d1261e311b16979dbc8eb34542f3966d82e"
)

// tmuxBuildScript builds tmux from the official release into ~/.local on the
// cluster. libevent is copied next to it (found through an $ORIGIN rpath) so
// the binary also runs on compute nodes that lack the library.
var tmuxBuildScript = `set -euo pipefail
V=` + tmuxVersion + `
SUM=` + tmuxSHA256 + `
PREFIX="$HOME/.local"
W=$(mktemp -d "${TMPDIR:-/tmp}/cluster-tmux.XXXXXX")
trap 'rm -rf "$W"' EXIT
cd "$W"
echo "Downloading tmux $V..."
if command -v curl >/dev/null 2>&1; then
  curl -fsSL -o tmux.tar.gz "https://github.com/tmux/tmux/releases/download/$V/tmux-$V.tar.gz"
else
  wget -q -O tmux.tar.gz "https://github.com/tmux/tmux/releases/download/$V/tmux-$V.tar.gz"
fi
echo "$SUM  tmux.tar.gz" | sha256sum -c --quiet -
tar xzf tmux.tar.gz
cd "tmux-$V"
echo "Configuring..."
./configure --prefix="$PREFIX" >configure.log 2>&1 || { tail -n 25 configure.log; exit 1; }
echo "Compiling (about a minute)..."
make -j4 LDFLAGS='-Wl,-rpath,\$$ORIGIN/../lib/cluster-tmux' >make.log 2>&1 || { tail -n 25 make.log; exit 1; }
mkdir -p "$PREFIX/bin" "$PREFIX/lib/cluster-tmux" "$PREFIX/share/man/man1"
ldd ./tmux | awk '/libevent/ { print $3 }' | while read -r lib; do cp -L "$lib" "$PREFIX/lib/cluster-tmux/"; done
install -m 755 tmux "$PREFIX/bin/tmux"
install -m 644 tmux.1 "$PREFIX/share/man/man1/tmux.1"
echo "Installed $("$PREFIX/bin/tmux" -V) at $PREFIX/bin/tmux"
`

// probeScript reports what the cluster has, one key=value per line.
const probeScript = `T=$(command -v tmux 2>/dev/null || true)
if [ -z "$T" ] && [ -x "$HOME/.local/bin/tmux" ]; then T="$HOME/.local/bin/tmux"; fi
echo "tmux=$T"
[ -n "$T" ] && echo "tmux_version=$("$T" -V 2>/dev/null)"
echo "slurm=$(srun --version 2>/dev/null)"
echo "jupyter=$(command -v jupyter 2>/dev/null)"
for t in gcc make tar sha256sum; do command -v $t >/dev/null 2>&1 || echo "missing=$t"; done
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || echo "missing=curl or wget"
`

func setupCmd(g *globals) *cobra.Command {
	var yes, skipTmux bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Check the cluster for what cluster needs, and offer to install tmux there",
		Long: `Check the cluster for what cluster relies on: Slurm's version, Jupyter, and
tmux. If tmux is missing, explain what it is for and offer to build it into
~/.local/bin on the cluster (no root needed). install.sh runs this for you.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			return runSetup(e, yes, skipTmux)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "install tmux without asking if it is missing")
	cmd.Flags().BoolVar(&skipTmux, "no-tmux", false, "don't offer to install tmux")
	return cmd
}

func runSetup(e *lifecycle.Env, yes, skipTmux bool) error {
	fmt.Printf("Checking %s...\n", e.Cfg.Host)
	if err := e.R.EnsureMaster(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	out, err := e.R.Output(ctx, "bash -s", strings.NewReader(probeScript))
	cancel()
	if err != nil {
		return err
	}
	facts := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			facts[k] = append(facts[k], v)
		}
	}
	get := func(k string) string {
		if v := facts[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}

	// Slurm
	version := strings.TrimPrefix(get("slurm"), "slurm ")
	switch major, minor, ok := parseSlurmVersion(version); {
	case !ok:
		fmt.Println("  slurm:   version unknown")
	case major < 20 || major == 20 && minor < 11:
		fmt.Printf("  slurm:   %s — older than 20.11; use --no-overlap with shell, dev and attach\n", version)
	default:
		fmt.Printf("  slurm:   %s ok\n", version)
	}

	// Jupyter
	if j := get("jupyter"); j != "" {
		fmt.Printf("  jupyter: %s\n", j)
	} else if e.Cfg.Jupyter.Setup != "" {
		fmt.Printf("  jupyter: not on the default PATH; your [jupyter] setup line will be run first\n")
	} else {
		fmt.Printf("  jupyter: not found — set [jupyter] setup in `cluster config` (e.g. module load / conda activate)\n")
	}

	// tmux
	if t := get("tmux"); t != "" {
		fmt.Printf("  tmux:    %s (%s)\n", get("tmux_version"), t)
		return nil
	}
	fmt.Println("  tmux:    not installed")
	if skipTmux {
		return nil
	}
	fmt.Print(`
tmux keeps your shell running on the compute node when your connection drops
(Wi-Fi, VPN, laptop sleep). With it, cluster reconnects you to the same shell
and anything you left running (a training run, an editor) is untouched.
Without it, a dropped connection ends the shell and kills what was running in
it; the node stays allocated and cluster reconnects you to a fresh shell.

cluster can build tmux ` + tmuxVersion + ` from the official source into ~/.local/bin on the
cluster. It needs no root, takes about a minute, and uses about 2 MB.
`)
	if missing := facts["missing"]; len(missing) > 0 {
		fmt.Printf("It can't be built here: the cluster lacks %s. Ask your admins for tmux.\n", strings.Join(missing, ", "))
		return nil
	}
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Println("Run `cluster setup` in a terminal to install it.")
			return nil
		}
		if !confirm("Install tmux on the cluster? [Y/n] ", true) {
			fmt.Println("Skipped. Run `cluster setup` any time to install it.")
			return nil
		}
	}

	build := e.R.Command([]string{"-T"}, "bash -s")
	build.Stdin = strings.NewReader(tmuxBuildScript)
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("building tmux failed: %w", err)
	}

	if !yes && term.IsTerminal(int(os.Stdin.Fd())) &&
		confirm("Check that it runs on a compute node? This submits a 1-minute, 1-CPU job. [Y/n] ", true) {
		return checkTmuxOnNode(e)
	}
	return nil
}

func checkTmuxOnNode(e *lifecycle.Env) error {
	base, err := e.Cfg.Profile("")
	if err != nil {
		return err
	}
	base.CPUs, base.Mem, base.Time, base.GPUs = 1, "500M", "00:01:00", 0
	req := slurm.Request{Slurm: base, Name: slurm.JobPrefix + "setup"}
	argv := append(append([]string{"srun"}, req.Args()...), "bash", "-c", `"$HOME/.local/bin/tmux" -V && hostname`)
	fmt.Printf("srun %s\n", req)
	out, err := e.R.Output(context.Background(), remote.Join(argv...), nil)
	if err != nil {
		return fmt.Errorf("tmux did not run on the compute node: %w", err)
	}
	lines := strings.Fields(strings.TrimSpace(out))
	fmt.Printf("tmux works on compute node %s.\n", lines[len(lines)-1])
	return nil
}

func confirm(prompt string, def bool) bool {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def
	case "y", "yes":
		return true
	}
	return false
}

var slurmVersionRE = regexp.MustCompile(`(\d+)\.(\d+)`)

func parseSlurmVersion(s string) (int, int, bool) {
	m := slurmVersionRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major, minor, true
}
