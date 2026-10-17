package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lunar-photon/cluster/internal/slurm"
)

func infoCmd(g *globals) *cobra.Command {
	return &cobra.Command{
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
			printInfo(os.Stdout, e.Cfg.Host, info)
			return nil
		},
	}
}

func printInfo(w io.Writer, host string, info *slurm.Info) {
	fmt.Fprintf(w, "%s@%s · %s\n", info.User, host, info.AccountText())
	for _, line := range info.UsageText() {
		fmt.Fprintf(w, "  %s\n", line)
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
