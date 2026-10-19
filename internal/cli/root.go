// Package cli defines the cluster command-line interface.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/lifecycle"
	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/session"
	"github.com/lunar-photon/cluster/internal/slurm"
	"github.com/lunar-photon/cluster/internal/tui"
)

var Version = "2.0.0-dev"

// globals holds the persistent flags shared by all commands.
type globals struct {
	configPath string
	host       string
	dryRun     bool
}

func (g *globals) load() (config.Config, error) {
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return cfg, err
	}
	if g.host != "" {
		cfg.Host = g.host
	}
	return cfg, nil
}

// env loads config and builds the lifecycle environment; it requires a host.
func (g *globals) env() (*lifecycle.Env, error) {
	cfg, err := g.load()
	if err != nil {
		return nil, err
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("no cluster host configured: run `cluster init`, pass --host, or set CLUSTER_HOST")
	}
	if cfg.Legacy && onceADay("legacy-config-note") {
		fmt.Fprintf(os.Stderr, "Note: using the old shell-style config; run `cluster config migrate` to switch to %s.\n", cfg.Path)
	}
	r := remote.New(cfg.Host)
	r.DryRun = g.dryRun
	e := lifecycle.New(cfg, r)
	if g.dryRun {
		// Keep dry runs from touching the real session list.
		dir, err := os.MkdirTemp("", "cluster-dry-run-")
		if err != nil {
			return nil, err
		}
		e.Store = &session.Store{Path: filepath.Join(dir, "sessions.json")}
	}
	return e, nil
}

// slurmFlags are the allocation flags shared by terminal, exec and jupyter.
type slurmFlags struct {
	profile   string
	gpu       bool
	partition string
	account   string
	qos       string
	cpus      int
	mem       string
	time      string
	gpus      int
	extra     []string
}

func (f *slurmFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVarP(&f.profile, "profile", "P", "", "config profile to start from (default: default_profile)")
	fl.BoolVarP(&f.gpu, "gpu", "g", false, "shorthand for --profile gpu")
	fl.StringVarP(&f.partition, "partition", "p", "", "Slurm partition")
	fl.StringVarP(&f.account, "account", "A", "", "Slurm account")
	fl.StringVarP(&f.qos, "qos", "q", "", "Slurm QOS")
	fl.IntVarP(&f.cpus, "cpus", "c", 0, "CPU cores")
	fl.StringVarP(&f.mem, "mem", "m", "", "memory, e.g. 16G")
	fl.StringVarP(&f.time, "time", "t", "", "walltime, e.g. 04:00:00")
	fl.IntVar(&f.gpus, "gpus", 0, "number of GPUs")
	fl.StringArrayVar(&f.extra, "slurm-arg", nil, "extra sbatch/srun argument (repeatable)")
}

// request resolves profile + flag overrides into a Slurm request.
func (f *slurmFlags) request(cfg config.Config, fallbackProfile string) (slurm.Request, string, error) {
	name := f.profile
	if f.gpu {
		name = "gpu"
	}
	if name == "" {
		name = fallbackProfile
	}
	base, err := cfg.Profile(name)
	if err != nil {
		if f.gpu {
			return slurm.Request{}, "", fmt.Errorf("no GPU profile in your config; run `cluster init` (it finds the GPU partitions) or add [profiles.gpu] with `cluster config`")
		}
		return slurm.Request{}, "", err
	}
	if name == "" {
		name = cfg.DefaultProfile
	}
	req := slurm.Request{Slurm: base.Merge(config.Slurm{
		Partition: f.partition, Account: f.account, QOS: f.qos,
		CPUs: f.cpus, Mem: f.mem, Time: f.time, GPUs: f.gpus, Extra: f.extra,
	})}
	return req, name, nil
}

func NewRoot() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "cluster",
		Short: "Work on a Slurm cluster from your laptop",
		Long: `cluster bridges a remote Slurm cluster and your workstation: interactive
compute shells that survive Wi-Fi drops, local mounts of cluster storage,
JupyterLab on compute nodes, code sync, and a live dashboard.

Run with no arguments in a terminal to open the dashboard.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
				return runTUI(g)
			}
			return cmd.Help()
		},
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.configPath, "config", "", "config file (default "+config.DefaultPath()+")")
	pf.StringVarP(&g.host, "host", "H", "", "cluster host or ~/.ssh/config alias")
	pf.BoolVarP(&g.dryRun, "dry-run", "n", false, "print remote and local commands instead of running them")

	root.AddCommand(
		terminalCmd(g),
		devCmd(g),
		attachCmd(g),
		psCmd(g),
		stopCmd(g),
		gcCmd(g),
		jupyterCmd(g),
		mountCmd(g),
		unmountCmd(g),
		syncCmd(g),
		statusCmd(g),
		execCmd(g),
		configCmd(g),
		initCmd(g),
		setupCmd(g),
		infoCmd(g),
		&cobra.Command{
			Use:   "connect",
			Short: "Open the shared ssh connection (prompts for password/2FA if needed)",
			Args:  cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				e, err := g.env()
				if err != nil {
					return err
				}
				if err := e.R.EnsureMaster(); err != nil {
					return err
				}
				fmt.Printf("Connected to %s; other commands reuse this connection for %s after last use.\n", e.R.Host, e.R.Persist)
				return nil
			},
		},
		&cobra.Command{
			Use:     "tui",
			Aliases: []string{"ui", "dashboard"},
			Short:   "Open the interactive dashboard",
			Args:    cobra.NoArgs,
			RunE:    func(*cobra.Command, []string) error { return runTUI(g) },
		},
	)
	return root
}

func runTUI(g *globals) error {
	cfg, err := g.load()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	var r *remote.Client
	if cfg.Host != "" {
		r = remote.New(cfg.Host)
		// Log in before the dashboard takes over the terminal, so password or
		// 2FA prompts work; the dashboard's queries then reuse the connection.
		if err := r.EnsureMaster(); err != nil {
			fmt.Fprintf(os.Stderr, "%v\nOpening the dashboard offline.\n", err)
		}
	}
	return tui.Run(cfg, r, self)
}

func Execute() int {
	if err := NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}

// onceADay reports true at most once every 24 hours per name, using a stamp
// file in the state directory. If the stamp can't be written it says true.
func onceADay(name string) bool {
	stamp := filepath.Join(session.Dir(), "."+name)
	if st, err := os.Stat(stamp); err == nil && time.Since(st.ModTime()) < 24*time.Hour {
		return false
	}
	_ = os.MkdirAll(session.Dir(), 0o700)
	_ = os.WriteFile(stamp, nil, 0o600)
	return true
}
