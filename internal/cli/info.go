package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/slurm"
)

func infoCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "info [PARTITION]",
		Aliases: []string{"partitions", "specs"},
		Short:   "Show partitions, node specs, free resources and your limits",
		Long: `Show what the cluster has and what you can use: for every partition, the
node hardware (cores, sockets, RAM, GPUs), how many nodes are idle, partly
used, full or unavailable, free CPUs and GPUs, the time limit, and whether
your account and QOS may use it. Your QOS limits and current usage are shown
at the top. Give a partition name for its full detail.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.env()
			if err != nil {
				return err
			}
			if err := e.R.EnsureMaster(); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			info, err := e.Slurm.ClusterInfo(ctx)
			if err != nil {
				return err
			}
			if p, err := e.Cfg.Profile(""); err == nil {
				info.PreferredQOS = p.QOS
			}
			if asJSON {
				j := clusterJSON(e.Cfg, info)
				if len(args) == 1 {
					for _, p := range j.Partitions {
						if p.Name == args[0] {
							return printJSON(p)
						}
					}
					return fmt.Errorf("no partition %q", args[0])
				}
				return printJSON(j)
			}
			if len(args) == 1 {
				for _, s := range info.Summarize() {
					if s.Name == args[0] {
						for _, line := range info.Detail(s) {
							fmt.Println(line)
						}
						return nil
					}
				}
				return fmt.Errorf("no partition %q", args[0])
			}
			printInfo(os.Stdout, e.Cfg, info)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return cmd
}

// ---- JSON shapes (stable field names) ----

type nodeTypeJSON struct {
	Count          int     `json:"count"`
	CPUs           int     `json:"cpus"`
	Sockets        int     `json:"sockets"`
	CoresPerSocket int     `json:"cores_per_socket"`
	ThreadsPerCore int     `json:"threads_per_core"`
	MemGB          float64 `json:"mem_gb"`
	GPUs           int     `json:"gpus"`
	GPUType        string  `json:"gpu_type,omitempty"`
}

type countJSON struct {
	Total int `json:"total"`
	Free  int `json:"free"`
}

type partitionJSON struct {
	Name           string   `json:"name"`
	Default        bool     `json:"default"`
	State          string   `json:"state"`
	Allowed        bool     `json:"allowed"`
	AllowedViaQOS  []string `json:"allowed_via_qos,omitempty"`
	WhyNot         string   `json:"why_not,omitempty"`
	JobQOS         string   `json:"job_qos,omitempty"`
	TimeLimit      string   `json:"time_limit"`
	MaxNodesPerJob string   `json:"max_nodes_per_job"`
	Nodes          struct {
		Total       int      `json:"total"`
		Idle        int      `json:"idle"`
		PartlyUsed  int      `json:"partly_used"`
		Full        int      `json:"full"`
		Unavailable int      `json:"unavailable"`
		IdleNames   []string `json:"idle_names"`
		DownNames   []string `json:"unavailable_names"`
	} `json:"nodes"`
	CPUs            countJSON      `json:"cpus"`
	GPUs            countJSON      `json:"gpus"`
	MostCoresOnNode int            `json:"most_free_cores_on_one_node"`
	AvailableToYou  *availableJSON `json:"available_to_you,omitempty"`
	NodeTypes       []nodeTypeJSON `json:"node_types"`
}

type availableJSON struct {
	CPUs      int    `json:"cpus"`
	Nodes     int    `json:"nodes"`
	GPUs      int    `json:"gpus"`
	LimitedBy string `json:"limited_by"`
}

type qosJSON struct {
	Name            string `json:"name"`
	MaxCPUsPerUser  int    `json:"max_cpus_per_user,omitempty"`
	MaxNodesPerUser int    `json:"max_nodes_per_user,omitempty"`
	MaxGPUsPerUser  int    `json:"max_gpus_per_user,omitempty"`
	MaxWall         string `json:"max_wall,omitempty"`
	UsedCPUs        int    `json:"used_cpus"`
	UsedNodes       int    `json:"used_nodes"`
	UsedGPUs        int    `json:"used_gpus"`
}

type policyJSON struct {
	Source       string   `json:"source"`
	JobPrefix    string   `json:"job_prefix,omitempty"`
	Partitions   []string `json:"partitions,omitempty"`
	MaxJobs      int      `json:"max_jobs,omitempty"`
	MaxCPUs      int      `json:"max_cpus_per_job,omitempty"`
	MaxTotalCPUs int      `json:"max_total_cpus,omitempty"`
	Rules        []string `json:"rules"`
}

// policySource names where the rules come from.
func policySource(cfg config.Config) string {
	switch {
	case cfg.Project != nil && !cfg.Project.Policy.Empty() && cfg.Policy != nil && !cfg.Policy.Empty():
		return "your config and " + config.ShortPath(cfg.Project.Path)
	case cfg.Project != nil && !cfg.Project.Policy.Empty():
		return config.ShortPath(cfg.Project.Path)
	}
	return "your config"
}

type clusterInfoJSON struct {
	Policy     *policyJSON     `json:"policy,omitempty"`
	User       string          `json:"user"`
	Host       string          `json:"host"`
	Accounts   []string        `json:"accounts"`
	QOS        []qosJSON       `json:"qos"`
	MemTracked bool            `json:"memory_reserved_per_job"`
	Partitions []partitionJSON `json:"partitions"`
}

func clusterJSON(cfg config.Config, info *slurm.Info) clusterInfoJSON {
	j := clusterInfoJSON{User: info.User, Host: cfg.Host, MemTracked: info.MemTracked, Accounts: []string{}, QOS: []qosJSON{}}
	if pol := cfg.EffectivePolicy(); !pol.Empty() {
		j.Policy = &policyJSON{Source: policySource(cfg), JobPrefix: pol.JobPrefix, Partitions: pol.Partitions,
			MaxJobs: pol.MaxJobs, MaxCPUs: pol.MaxCPUs, MaxTotalCPUs: pol.MaxTotalCPUs, Rules: pol.Lines()}
	}
	usage := info.Usage()
	seenA, seenQ := map[string]bool{}, map[string]bool{}
	for _, a := range info.Assocs {
		if !seenA[a.Account] {
			seenA[a.Account] = true
			j.Accounts = append(j.Accounts, a.Account)
		}
		for _, q := range a.QOS {
			if seenQ[q] {
				continue
			}
			seenQ[q] = true
			l := info.QOS[q].MaxTRESPerUserParsed
			u := usage[q]
			j.QOS = append(j.QOS, qosJSON{Name: q, MaxCPUsPerUser: l["cpu"], MaxNodesPerUser: l["node"],
				MaxGPUsPerUser: l["gres/gpu"], MaxWall: info.QOS[q].MaxWall, UsedCPUs: u.CPUs, UsedNodes: u.Nodes, UsedGPUs: u.GPUs})
		}
	}
	for _, s := range info.Summarize() {
		p := partitionJSON{Name: s.Name, Default: s.Default, State: s.State, Allowed: s.Allowed, AllowedViaQOS: s.AllowedVia,
			WhyNot: s.Why, TimeLimit: slurm.Unlimited(s.MaxTime), MaxNodesPerJob: slurm.Unlimited(s.MaxNodes),
			CPUs: countJSON{s.CPUsTotal, s.CPUsFree}, GPUs: countJSON{s.GPUsTotal, s.GPUsFree}, MostCoresOnNode: s.BestFreeCPUs}
		if s.Allowed {
			p.JobQOS = info.QOSFor(s)
			a := info.AvailableTo(s)
			p.AvailableToYou = &availableJSON{a.CPUs, a.Nodes, a.GPUs, a.LimitedBy}
		}
		p.Nodes.Total = len(s.Nodes)
		p.Nodes.Idle, p.Nodes.PartlyUsed, p.Nodes.Full, p.Nodes.Unavailable = s.Idle, s.Mixed, s.Allocated, s.Unavailable
		p.Nodes.IdleNames, p.Nodes.DownNames = nonNil(s.IdleNodes), nonNil(s.DownNodes)
		for _, sh := range s.Shapes {
			p.NodeTypes = append(p.NodeTypes, nodeTypeJSON{sh.Count, sh.CPUs, sh.Sockets, sh.CoresPerSocket,
				sh.ThreadsPerCore, float64(sh.MemMB) / 1024, sh.GPUs, sh.GPUType})
		}
		j.Partitions = append(j.Partitions, p)
	}
	return j
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func printInfo(w io.Writer, cfg config.Config, info *slurm.Info) {
	fmt.Fprintf(w, "%s@%s · %s\n", info.User, cfg.Host, info.AccountText())
	for _, line := range info.UsageText() {
		fmt.Fprintf(w, "  %s\n", line)
	}
	if pol := cfg.EffectivePolicy(); !pol.Empty() {
		fmt.Fprintf(w, "Rules (%s): %s\n", policySource(cfg), strings.Join(pol.Lines(), " · "))
	}
	fmt.Fprintln(w)
	printPartitionTable(w, info)
	if !info.MemTracked {
		fmt.Fprintln(w, "\nMemory isn't reserved per job on this cluster; RAM figures are per node.")
	}
	fmt.Fprintln(w, "Nodes column: idle/partly used/full/unavailable. `cluster info PARTITION` for details.")
}

func printPartitionTable(w io.Writer, info *slurm.Info) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PARTITION\tACCESS\tTIME LIMIT\tNODES\tFREE CPUs\tFREE GPUs\tFOR YOU NOW\tNODE TYPE")
	for _, s := range info.Summarize() {
		name := s.Name
		if s.Default {
			name += "*"
		}
		gpus := "-"
		if s.GPUsTotal > 0 {
			gpus = fmt.Sprintf("%d/%d", s.GPUsFree, s.GPUsTotal)
		}
		mine := "-"
		if s.Allowed {
			a := info.AvailableTo(s)
			mine = slurm.Plural(a.CPUs, "CPU")
			if s.GPUsTotal > 0 {
				mine += ", " + slurm.Plural(a.GPUs, "GPU")
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d\t%s\t%s\t%s\n", name, s.AccessText(), slurm.Unlimited(s.MaxTime),
			s.StateText(), s.CPUsFree, s.CPUsTotal, gpus, mine, s.ShapeText())
	}
	tw.Flush()
}
