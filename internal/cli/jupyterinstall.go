package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/lifecycle"
	"github.com/lunar-photon/cluster/internal/remote"
)

// Where `cluster setup` installs JupyterLab on the cluster (a venv in the
// shared home), and where the downloaded wheels wait to be installed.
const (
	jupyterEnv     = ".local/share/cluster/jupyter"
	jupyterWheels  = ".cluster/jupyter-wheels"
	jupyterActLine = `source "$HOME/` + jupyterEnv + `/bin/activate"`
)

// pythonCandidate is a Python that runs on a compute node, possibly only
// after loading an environment module.
type pythonCandidate struct {
	Module       string // "" when it's on the default PATH
	Path         string
	Major, Minor int
}

// load is the shell prefix that makes this Python available.
func (p pythonCandidate) load() string {
	if p.Module == "" {
		return ""
	}
	return "module load " + remote.Quote(p.Module) + " && "
}

// pythonProbeScript runs on a compute node in a login shell. It lists every
// Python that actually runs there ("python=MODULE|PATH|X.Y"), on the default
// PATH and from each python/* environment module, plus the node's glibc and
// architecture. A Python is only listed if it starts, which weeds out builds
// missing their libraries.
const pythonProbeScript = `cand() {
  for p in python3.13 python3.12 python3.11 python3.10 python3.9 python3; do
    q=$(command -v $p 2>/dev/null) || continue
    v=$("$q" -c 'import sys; print("%d.%d" % sys.version_info[:2])' 2>/dev/null) || continue
    echo "python=$1|$q|$v"
  done
}
cand ""
if command -v module >/dev/null 2>&1; then
  for m in $(module -t avail python 2>&1 | grep -E '^python[^/ ]*/[0-9]' | sed 's/([^)]*)//g'); do
    ( module load "$m" >/dev/null 2>&1 && cand "$m" )
  done
fi
echo "glibc=$(ldd --version 2>/dev/null | head -1 | grep -oE '[0-9]+\.[0-9]+$')"
echo "arch=$(uname -m)"
`

// bestPython picks the newest Python 3.9+ from probe lines
// ("MODULE|PATH|X.Y"), preferring one that needs no module.
func bestPython(lines []string) (pythonCandidate, bool) {
	var best pythonCandidate
	found := false
	for _, l := range lines {
		f := strings.Split(l, "|")
		if len(f) != 3 {
			continue
		}
		major, minor, ok := parseSlurmVersion(f[2])
		if !ok || major != 3 || minor < 9 {
			continue
		}
		c := pythonCandidate{f[0], f[1], major, minor}
		better := !found || minor > best.Minor || (minor == best.Minor && best.Module != "" && c.Module == "")
		if better {
			best, found = c, true
		}
	}
	return best, found
}

// manylinuxTags are the wheel platform tags a glibc version can run, e.g.
// glibc 2.28 on x86_64 → manylinux_2_28_x86_64 … manylinux_2_5_x86_64 and
// the older manylinux2014/2010/1 aliases.
func manylinuxTags(glibc, arch string) []string {
	major, minor, ok := parseSlurmVersion(glibc)
	if !ok || major != 2 {
		minor = 17 // unknown: assume the common baseline
	}
	var tags []string
	for m := minor; m >= 5; m-- {
		tags = append(tags, fmt.Sprintf("manylinux_2_%d_%s", m, arch))
	}
	for _, legacy := range []struct {
		name string
		min  int
	}{{"manylinux2014", 17}, {"manylinux2010", 12}, {"manylinux1", 5}} {
		if minor >= legacy.min {
			tags = append(tags, legacy.name+"_"+arch)
		}
	}
	return tags
}

// pipDownloadArgs downloads wheels for another machine's Python and platform.
func pipDownloadArgs(dest string, py pythonCandidate, glibc, arch string, pkgs []string) []string {
	v := fmt.Sprintf("%d%d", py.Major, py.Minor)
	// Patient with slow connections: PyPI downloads can stall for a while.
	args := []string{"-m", "pip", "download", "--quiet", "--timeout", "60", "--retries", "10", "--dest", dest, "--only-binary=:all:",
		"--python-version", fmt.Sprintf("%d.%d", py.Major, py.Minor), "--implementation", "cp",
		"--abi", "cp" + v, "--abi", "abi3", "--abi", "none"}
	for _, t := range manylinuxTags(glibc, arch) {
		args = append(args, "--platform", t)
	}
	return append(args, pkgs...)
}

// jupyterInstallScript runs on a compute node: it makes a venv with the
// cluster's Python and installs from the copied wheels, with no network.
// pip comes along as a wheel because some clusters' Pythons lack it.
func jupyterInstallScript(py pythonCandidate, pkgs []string) string {
	load := ""
	if py.Module != "" {
		load = "module load " + remote.Quote(py.Module) + "\n"
	}
	return `set -euo pipefail
` + load + `V="$HOME/` + jupyterEnv + `"
W="$HOME/` + jupyterWheels + `"
echo "Installing on $(hostname) with $(` + remote.Quote(py.Path) + ` --version 2>&1):"
rm -rf "$V"
` + remote.Quote(py.Path) + ` -m venv --without-pip "$V"
PIP=$(ls "$W"/pip-*.whl | head -1)
"$V/bin/python" "$PIP/pip" install --quiet --no-index --find-links "$W" ` + remote.Join(pkgs...) + `
echo "  $("$V/bin/jupyter" lab --version | sed 's/^/JupyterLab /') in ~/` + jupyterEnv + `"
rm -rf "$W"
`
}

// setupJupyter offers to install JupyterLab when the cluster has none (or
// when asked to), the same way setup offers tmux.
func setupJupyter(e *lifecycle.Env, facts map[string][]string, yes, force bool, with []string) error {
	first := func(k string) string {
		if v := facts[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if !force && (first("jupyter") != "" || e.Cfg.Jupyter.Setup != "") {
		return nil // Jupyter is there, or the user's setup line provides it
	}
	req, err := tmuxBuildRequest(e) // the same short job the tmux build uses
	if err != nil {
		return err
	}
	if !force {
		fmt.Printf(`
JupyterLab isn't available on the cluster. cluster can install it for you:
it finds a Python 3.9+ that runs on the compute nodes (with a short job),
this computer downloads JupyterLab for it, copies it over, and a compute node
installs it into ~/%s (%d CPUs, %s, up to %s). No root, and the cluster needs
no internet. Add packages for your notebooks with --with numpy,matplotlib.
`, jupyterEnv, req.CPUs, req.Mem, req.Time)
	}
	if !yes && !force {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Println("Not asking without a terminal; run `cluster setup --yes` to install it.")
			return nil
		}
		if !confirm("Install JupyterLab on the cluster? [Y/n] ", true) {
			fmt.Println("Skipped. Run `cluster setup` any time to install it.")
			return nil
		}
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := e.CheckRequest(ctx, req)
		cancel()
		if err != nil {
			return err
		}
	}

	// 0. Find a Python on a compute node: login nodes often have others.
	fmt.Println("\nLooking for Python on a compute node...")
	probe := req
	probe.CPUs, probe.Mem, probe.Time = 1, "500M", "00:02:00"
	argv := append(append([]string{"srun"}, probe.Args()...), "bash", "-lc", pythonProbeScript)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	out, err := e.R.Output(ctx, remote.Join(argv...), nil)
	cancel()
	if err != nil {
		return fmt.Errorf("probing a compute node: %w", err)
	}
	node := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			node[k] = append(node[k], v)
		}
	}
	py, ok := bestPython(node["python"])
	if !ok {
		fmt.Println("No Python 3.9 or newer runs on the compute nodes (on the default PATH or from a python module).")
		fmt.Println("Set [jupyter] setup in `cluster config` to an environment that has Jupyter, or ask your admins.")
		return nil
	}
	where := py.Path
	if py.Module != "" {
		where += " (module " + py.Module + ")"
	}
	fmt.Printf("  using Python %d.%d: %s\n", py.Major, py.Minor, where)
	glibc, arch := "", ""
	if v := node["glibc"]; len(v) > 0 {
		glibc = v[0]
	}
	if v := node["arch"]; len(v) > 0 {
		arch = v[0]
	}
	pkgs := append([]string{"pip", "jupyterlab", "ipykernel"}, with...)

	// 1. Download on this computer.
	if err := exec.Command("python3", "-m", "pip", "--version").Run(); err != nil {
		return fmt.Errorf("downloading JupyterLab needs pip on this computer (e.g. `sudo pacman -S python-pip` or `sudo apt install python3-pip`)")
	}
	dir, err := os.MkdirTemp("", "cluster-jupyter-wheels-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	fmt.Printf("\nDownloading %s for Python %d.%d on %s (glibc %s) on this computer...\n",
		strings.Join(pkgs[1:], ", "), py.Major, py.Minor, arch, glibc)
	dl := exec.Command("python3", pipDownloadArgs(dir, py, glibc, arch, pkgs)...)
	dl.Stdout, dl.Stderr = os.Stdout, os.Stderr
	if err := dl.Run(); err != nil {
		return fmt.Errorf("downloading the wheels failed: %w", err)
	}
	entries, _ := os.ReadDir(dir)
	var size int64
	for _, en := range entries {
		if info, err := en.Info(); err == nil {
			size += info.Size()
		}
	}
	fmt.Printf("  %d packages, %s\n", len(entries), humanBytes(size))

	// 2. Copy to the cluster.
	fmt.Println("Copying them to the cluster...")
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	_, err = e.R.Output(ctx, "mkdir -p \"$HOME\"/"+jupyterWheels, nil)
	cancel()
	if err != nil {
		return err
	}
	if err := e.R.Local("rsync", "-a", "--delete", "-e", e.R.SSHCommandLine(), dir+"/", e.Cfg.Host+":"+jupyterWheels+"/"); err != nil {
		return fmt.Errorf("copying the wheels failed: %w", err)
	}

	// 3. Install on a compute node.
	fmt.Printf("Submitting the install to a compute node (srun %s):\n", req)
	argv = append(append([]string{"srun"}, req.Args()...), "bash", "-lc", jupyterInstallScript(py, pkgs))
	inst := e.R.Command([]string{"-T"}, remote.Join(argv...))
	inst.Stdout, inst.Stderr = os.Stdout, os.Stderr
	if err := inst.Run(); err != nil {
		return fmt.Errorf("installing JupyterLab on the compute node failed: %w", err)
	}

	// 4. Point cluster jupyter at it, unless the user chose their own setup.
	line := py.load() + jupyterActLine
	switch e.Cfg.Jupyter.Setup {
	case line:
	case "":
		cfg, err := config.Load(e.Cfg.Path)
		if err != nil {
			return err
		}
		cfg.Jupyter.Setup = line
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("Set [jupyter] setup in %s, so cluster jupyter uses it.\n", config.ShortPath(cfg.Path))
	default:
		fmt.Printf("Your [jupyter] setup is %q; to use this install instead, set it to:\n  %s\n", e.Cfg.Jupyter.Setup, line)
	}
	return nil
}

func humanBytes(n int64) string {
	if n >= 1<<20 {
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	}
	return strconv.FormatInt(n/1024, 10) + " KB"
}
