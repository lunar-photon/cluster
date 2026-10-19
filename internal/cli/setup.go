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

// Sources `cluster setup` builds tmux from, pinned by SHA-256. libevent and
// ncurses are built too, because compute nodes typically lack their headers;
// linking them statically leaves tmux depending only on the C library.
type tmuxSource struct{ name, url, sha256 string }

var tmuxSources = []tmuxSource{
	{"tmux", "https://github.com/tmux/tmux/releases/download/3.8/tmux-3.8.tar.gz",
		"e79c699c7e949dccd0a4a125e17b8d1261e311b16979dbc8eb34542f3966d82e"},
	{"libevent", "https://github.com/libevent/libevent/releases/download/release-2.1.13-stable/libevent-2.1.13-stable.tar.gz",
		"f7e9383b8c0baa81b687e5b5eecc01beefaf1b19b64151d95ed61647fe7a315c"},
	// Signed by Thomas E. Dickey, key 19882D92DDA4C400C22C0D56CC2AF4472167BE03.
	{"ncurses", "https://ftp.gnu.org/gnu/ncurses/ncurses-6.6.tar.gz",
		"355b4cbbed880b0381a04c46617b7656e362585d52e9cf84a67e2009b749ff11"},
}

const tmuxVersion = "3.8"

// tmuxBuildDir is in the shared home, so the compute node sees what the
// login node downloaded.
const tmuxBuildDir = "$HOME/.cluster/tmux-build"

// tmuxFetchScript runs on the login node, which has internet access. It
// only downloads and verifies; nothing is compiled there.
func tmuxFetchScript() string {
	var b strings.Builder
	b.WriteString(`set -euo pipefail
B="` + tmuxBuildDir + `"
rm -rf "$B" && mkdir -p "$B" && cd "$B"
fetch() {
  if command -v curl >/dev/null 2>&1; then curl -fsSL -o "$1" "$2"; else wget -q -O "$1" "$2"; fi
  echo "$3  $1" | sha256sum -c --quiet -
}
`)
	for _, src := range tmuxSources {
		fmt.Fprintf(&b, "echo \"  %s\"\nfetch %s.tar.gz %s %s\n", src.name, src.name, src.url, src.sha256)
	}
	return b.String()
}

// tmuxCompileScript runs on a compute node through srun. It builds static
// libevent and ncurses, then tmux against them, and installs tmux into
// ~/.local/bin. Logs stay in the build directory if a step fails.
const tmuxCompileScript = `set -euo pipefail
B="` + tmuxBuildDir + `"
P="$B/deps"
PREFIX="$HOME/.local"
J="${SLURM_CPUS_PER_TASK:-2}"
cd "$B"
for f in tmux libevent ncurses; do mkdir -p "src-$f" && tar xzf "$f.tar.gz" -C "src-$f" --strip-components=1; done
step() { # step NAME DIR COMMAND...
  local name=$1 dir=$2; shift 2
  echo "  $name"
  if ! (cd "$dir" && "$@") >"$B/$name.log" 2>&1; then
    echo "$name failed on $(hostname); last lines of $B/$name.log:" >&2
    tail -n 25 "$B/$name.log" >&2
    exit 1
  fi
}
echo "Building on $(hostname) with $J CPUs:"
step libevent src-libevent sh -c "./configure --prefix='$P' --disable-shared --enable-static --disable-openssl --disable-samples --disable-libevent-regress && make -j$J && make install"
# install.libs/includes only: the node already has a terminfo database, and
# installing ours would try to write to /usr/share/terminfo.
step ncurses src-ncurses sh -c "./configure --prefix='$P' --without-shared --without-debug --without-ada --without-cxx --without-cxx-binding --without-manpages --without-progs --without-tests --enable-widec --enable-pc-files --with-pkg-config-libdir='$P/lib/pkgconfig' --with-default-terminfo-dir=/usr/share/terminfo --with-terminfo-dirs=/etc/terminfo:/lib/terminfo:/usr/share/terminfo && make -j$J && make install.libs install.includes"
step tmux src-tmux sh -c "PKG_CONFIG_LIBDIR='$P/lib/pkgconfig' ./configure --prefix='$PREFIX' && make -j$J"
mkdir -p "$PREFIX/bin" "$PREFIX/share/man/man1"
install -m 755 src-tmux/tmux "$PREFIX/bin/tmux"
install -m 644 src-tmux/tmux.1 "$PREFIX/share/man/man1/tmux.1"
rm -rf "$PREFIX/lib/cluster-tmux" # libevent copy from older builds; now linked statically
cd && rm -rf "$B"
echo "Installed $("$PREFIX/bin/tmux" -V) at $PREFIX/bin/tmux (built on $(hostname))"
`

// probeScript reports what the cluster has, one key=value per line.
const probeScript = `T=$(command -v tmux 2>/dev/null || true)
if [ -z "$T" ] && [ -x "$HOME/.local/bin/tmux" ]; then T="$HOME/.local/bin/tmux"; fi
echo "tmux=$T"
[ -n "$T" ] && echo "tmux_version=$("$T" -V 2>/dev/null)"
echo "slurm=$(srun --version 2>/dev/null)"
echo "jupyter=$(command -v jupyter 2>/dev/null)"
for t in tar sha256sum; do command -v $t >/dev/null 2>&1 || echo "missing=$t"; done
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || echo "missing=curl or wget"
`

func setupCmd(g *globals) *cobra.Command {
	var yes, skipTmux, reinstall bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Check the cluster for what cluster needs, and offer to install tmux there",
		Long: `Check the cluster for what cluster relies on: Slurm's version, Jupyter, and
tmux. If tmux is missing, explain what it is for and offer to build it into
~/.local/bin on the cluster (no root needed). The login node only downloads
the sources; the build runs on a compute node. install.sh runs this for you.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			return runSetup(e, yes, skipTmux, reinstall)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "install tmux without asking if it is missing")
	cmd.Flags().BoolVar(&skipTmux, "no-tmux", false, "don't offer to install tmux")
	cmd.Flags().BoolVar(&reinstall, "reinstall-tmux", false, "build and install tmux even if one is already there")
	return cmd
}

func runSetup(e *lifecycle.Env, yes, skipTmux, reinstall bool) error {
	fmt.Printf("Checking %s...\n", e.Cfg.Host)
	if err := e.R.EnsureMaster(); err != nil {
		return err
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		notices := e.R.Notices(ctx)
		cancel()
		if len(notices) > 0 {
			fmt.Println("  ssh:     the server prints these on every connection (cluster hides them):")
			for _, n := range notices {
				fmt.Println("           " + n)
			}
		}
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
		fmt.Printf("  slurm:   %s — older than 20.11; use --no-overlap with terminal and attach\n", version)
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
		if !reinstall {
			return nil
		}
	} else {
		fmt.Println("  tmux:    not installed")
	}
	if skipTmux {
		return nil
	}
	req, err := tmuxBuildRequest(e)
	if err != nil {
		return err
	}
	if !reinstall {
		fmt.Print(`
tmux keeps your shell running on the compute node when your connection drops
(Wi-Fi, VPN, laptop sleep). With it, cluster reconnects you to the same shell
and anything you left running (a training run, an editor) is untouched.
Without it, a dropped connection ends the shell and kills what was running in
it; the node stays allocated and cluster reconnects you to a fresh shell.

cluster can build tmux ` + tmuxVersion + ` from the official sources into ~/.local/bin on the
cluster, with no root. The login node only downloads the sources (about 6 MB);
the build runs on a compute node as a short job (` + fmt.Sprintf("%d CPUs, %s, up to %s", req.CPUs, req.Mem, req.Time) + `,
usually 2-3 minutes). The result uses about 2 MB.
`)
	}
	if missing := facts["missing"]; len(missing) > 0 {
		fmt.Printf("It can't be built here: the login node lacks %s. Ask your admins for tmux.\n", strings.Join(missing, ", "))
		return nil
	}
	if !yes && !reinstall {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Println("Run `cluster setup` in a terminal to install it.")
			return nil
		}
		if !confirm("Install tmux on the cluster? [Y/n] ", true) {
			fmt.Println("Skipped. Run `cluster setup` any time to install it.")
			return nil
		}
	}
	return buildTmux(e, req)
}

// tmuxBuildRequest is a small allocation on the default profile's partition.
func tmuxBuildRequest(e *lifecycle.Env) (slurm.Request, error) {
	base, err := e.Cfg.Profile("")
	if err != nil {
		return slurm.Request{}, err
	}
	base.CPUs, base.Mem, base.Time, base.GPUs = 4, "2G", "00:20:00", 0
	return slurm.Request{Slurm: base, Name: e.Cfg.JobName("setup")}, nil
}

func buildTmux(e *lifecycle.Env, req slurm.Request) error {
	fmt.Println("\nDownloading and verifying sources on the login node:")
	fetch := e.R.Command([]string{"-T"}, "bash -s")
	fetch.Stdin = strings.NewReader(tmuxFetchScript())
	fetch.Stdout, fetch.Stderr = os.Stdout, os.Stderr
	if err := fetch.Run(); err != nil {
		return fmt.Errorf("downloading the tmux sources failed: %w", err)
	}

	fmt.Printf("Submitting the build to a compute node (srun %s):\n", req)
	argv := append(append([]string{"srun"}, req.Args()...), "bash", "-c", tmuxCompileScript)
	build := e.R.Command([]string{"-T"}, remote.Join(argv...))
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("building tmux on the compute node failed: %w", err)
	}
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
