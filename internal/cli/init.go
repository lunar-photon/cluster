package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/lifecycle"
	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/slurm"
)

func initCmd(g *globals) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up the config by asking the cluster what you're allowed to use",
		Long: `Interactively create the config file. Host suggestions come from
~/.ssh/config; accounts, QOS and partitions come from the cluster itself
(sacctmgr and sinfo), so you pick from what you actually have access to.

Without a terminal it doesn't ask: pass --yes to accept the values it
detects (with --host if no host is configured yet).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			var in *bufio.Reader // nil: accept every suggested value
			if !yes {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("cluster init asks questions and there's no terminal; pass --yes to accept the detected values")
				}
				in = bufio.NewReader(os.Stdin)
			}
			if cfg.Legacy {
				fmt.Println("Starting from your old shell-style config.")
			}

			if hosts := sshConfigHosts(); len(hosts) > 0 {
				fmt.Printf("Hosts in ~/.ssh/config: %s\n", strings.Join(hosts, ", "))
			}
			cfg.Host = ask(in, "Cluster host (hostname, user@hostname, or ~/.ssh/config alias)", firstNonEmpty(g.host, cfg.Host))
			if cfg.Host == "" {
				return fmt.Errorf("a host is required")
			}

			r := remote.New(cfg.Host)
			fmt.Printf("Connecting to %s...\n", cfg.Host)
			if err := r.EnsureMaster(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			assoc := probeAssociations(ctx, r)
			parts := probePartitions(ctx, r)

			accounts := uniq(assoc, 0)
			if len(accounts) > 0 {
				fmt.Printf("Your Slurm accounts: %s\n", strings.Join(accounts, ", "))
			}
			cfg.Slurm.Account = ask(in, "Default account (blank for none)", firstNonEmpty(cfg.Slurm.Account, single(accounts)))

			if info, err := slurm.New(r).ClusterInfo(ctx); err == nil && len(info.Partitions) > 0 {
				fmt.Println("\nPartitions on this cluster:")
				printPartitionTable(os.Stdout, info)
				fmt.Println()
			} else if len(parts) > 0 {
				names := make([]string, len(parts))
				for i, p := range parts {
					names[i] = p.name
					if p.gpu {
						names[i] += " (GPU)"
					}
				}
				fmt.Printf("Partitions: %s\n", strings.Join(names, ", "))
			}
			cfg.Slurm.Partition = ask(in, "CPU partition", firstNonEmpty(cfg.Slurm.Partition, defaultPartition(parts)))

			qos := qosFor(assoc, cfg.Slurm.Account)
			if len(qos) > 0 {
				fmt.Printf("QOS available to %s: %s\n", firstNonEmpty(cfg.Slurm.Account, "you"), strings.Join(qos, ", "))
			}
			cfg.Slurm.QOS = ask(in, "CPU QOS (blank for default)", cfg.Slurm.QOS)
			cfg.Slurm.CPUs = askInt(in, "CPU cores", cfg.Slurm.CPUs)
			cfg.Slurm.Mem = ask(in, "Memory per job, e.g. 8G (blank for the partition default)", cfg.Slurm.Mem)
			cfg.Slurm.Time = ask(in, "Time limit per job, e.g. 04:00:00 (blank for the partition limit)", cfg.Slurm.Time)

			gpu := cfg.Profiles["gpu"]
			gpu.Partition = ask(in, "GPU partition (blank to skip)", firstNonEmpty(gpu.Partition, gpuPartition(parts)))
			if gpu.Partition != "" {
				gpu.QOS = ask(in, "GPU QOS (blank to inherit)", gpu.QOS)
				if gpu.GPUs == 0 {
					gpu.GPUs = 1
				}
				gpu.GPUs = askInt(in, "GPUs per job", gpu.GPUs)
				cfg.Profiles["gpu"] = gpu
			} else {
				delete(cfg.Profiles, "gpu")
			}
			if _, ok := cfg.Profiles["cpu"]; !ok {
				cfg.Profiles["cpu"] = config.Slurm{}
			}

			cfg.MountPoint = ask(in, "Local mount point", cfg.MountPoint)
			cfg.Jupyter.Setup = ask(in, "Shell to run before Jupyter, e.g. `module load anaconda3` (blank for none)", cfg.Jupyter.Setup)

			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Printf("\nSaved %s.\n\n", cfg.Path)
			env := lifecycle.New(cfg, r)
			facts, err := runSetup(env, false, false, false)
			if err != nil {
				return err
			}
			if err := setupJupyter(env, facts, false, false, nil); err != nil {
				return err
			}
			fmt.Println("\nReady. Try `cluster terminal`, or just `cluster` for the dashboard.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "accept the detected values without asking")
	return cmd
}

// ask prompts for a value; with in == nil (--yes) it takes the default.
func ask(in *bufio.Reader, prompt, def string) string {
	if in == nil {
		fmt.Printf("%s: %s\n", prompt, def)
		return def
	}
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	if line == "-" { // explicit way to clear a default
		return ""
	}
	return line
}

func askInt(in *bufio.Reader, prompt string, def int) int {
	if in == nil {
		return def
	}
	for {
		s := ask(in, prompt, strconv.Itoa(def))
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return n
		}
		fmt.Println("Please enter a whole number.")
	}
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func single(list []string) string {
	if len(list) == 1 {
		return list[0]
	}
	return ""
}

// sshConfigHosts lists concrete Host aliases from ~/.ssh/config.
func sshConfigHosts() []string {
	home, _ := os.UserHomeDir()
	f, err := os.Open(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var hosts []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "host") {
			continue
		}
		for _, h := range fields[1:] {
			if !strings.ContainsAny(h, "*?!") {
				hosts = append(hosts, h)
			}
		}
	}
	return hosts
}

// probeAssociations returns account|partition|qos rows for the user.
func probeAssociations(ctx context.Context, r *remote.Client) [][]string {
	out, err := r.Output(ctx, `sacctmgr -nP show assoc user="$USER" format=account,partition,qos`, nil)
	if err != nil {
		return nil
	}
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Split(strings.TrimSpace(line), "|"); len(f) == 3 {
			rows = append(rows, f)
		}
	}
	return rows
}

func uniq(rows [][]string, col int) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if v := r[col]; v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func qosFor(rows [][]string, account string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if account != "" && r[0] != account {
			continue
		}
		for _, q := range strings.Split(r[2], ",") {
			if q != "" && !seen[q] {
				seen[q] = true
				out = append(out, q)
			}
		}
	}
	sort.Strings(out)
	return out
}

type partInfo struct {
	name      string
	isDefault bool
	gpu       bool
}

func probePartitions(ctx context.Context, r *remote.Client) []partInfo {
	out, err := r.Output(ctx, `sinfo -h -o '%P|%G'`, nil)
	if err != nil {
		return nil
	}
	byName := map[string]*partInfo{}
	var order []string
	for _, line := range strings.Split(out, "\n") {
		name, gres, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok {
			continue
		}
		def := strings.HasSuffix(name, "*")
		name = strings.TrimSuffix(name, "*")
		p, seen := byName[name]
		if !seen {
			p = &partInfo{name: name}
			byName[name] = p
			order = append(order, name)
		}
		p.isDefault = p.isDefault || def
		p.gpu = p.gpu || strings.Contains(gres, "gpu")
	}
	parts := make([]partInfo, 0, len(order))
	for _, n := range order {
		parts = append(parts, *byName[n])
	}
	return parts
}

func defaultPartition(parts []partInfo) string {
	for _, p := range parts {
		if p.isDefault {
			return p.name
		}
	}
	return ""
}

func gpuPartition(parts []partInfo) string {
	for _, p := range parts {
		if p.gpu {
			return p.name
		}
	}
	return ""
}
