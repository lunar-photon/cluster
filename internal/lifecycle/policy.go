package lifecycle

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/lunar-photon/cluster/internal/slurm"
)

// PolicyError is a request the configured usage rules don't allow.
type PolicyError struct{ Msg string }

func (e *PolicyError) Error() string { return "policy: " + e.Msg }

// CheckPolicy refuses a request that breaks the effective policy (the
// user's [policy] plus the project's .cluster.toml). It counts the user's
// active jobs on the cluster for max_jobs and max_total_cpus.
func (e *Env) CheckPolicy(ctx context.Context, req slurm.Request) error {
	pol := e.Cfg.EffectivePolicy()
	if pol.Empty() {
		return nil
	}
	if pol.JobPrefix != "" && !strings.HasPrefix(req.Name, pol.JobPrefix) {
		return &PolicyError{fmt.Sprintf("job names must start with %q; this one is %q", pol.JobPrefix, req.Name)}
	}
	cpus := max(1, req.CPUs)
	if pol.MaxCPUs > 0 && cpus > pol.MaxCPUs {
		return &PolicyError{fmt.Sprintf("at most %d CPUs per job; this asks for %d", pol.MaxCPUs, cpus)}
	}
	if len(pol.Partitions) > 0 {
		part := req.Partition
		if part == "" && !e.R.DryRun {
			p, err := e.Slurm.DefaultPartition(ctx)
			if err != nil {
				return err
			}
			part = p
		}
		if part != "" && !containsString(pol.Partitions, part) {
			return &PolicyError{fmt.Sprintf("partition %s isn't allowed (allowed: %s)", part, strings.Join(pol.Partitions, ", "))}
		}
	}
	if (pol.MaxJobs == 0 && pol.MaxTotalCPUs == 0) || e.R.DryRun {
		return nil
	}
	jobs, err := e.Slurm.Jobs(ctx)
	if err != nil {
		return err
	}
	running, pending, used := 0, 0, 0
	for _, j := range jobs {
		if !slurm.IsActive(j.State) || (pol.JobPrefix != "" && !strings.HasPrefix(j.Name, pol.JobPrefix)) {
			continue
		}
		if j.State == "PENDING" {
			pending++
		} else {
			running++
		}
		n, _ := strconv.Atoi(j.CPUs)
		used += n
	}
	scope := "jobs"
	if pol.JobPrefix != "" {
		scope = pol.JobPrefix + "* jobs"
	}
	if pol.MaxJobs > 0 && running+pending >= pol.MaxJobs {
		return &PolicyError{fmt.Sprintf("at most %d %s at once; %d active (%d running, %d pending)",
			pol.MaxJobs, scope, running+pending, running, pending)}
	}
	if pol.MaxTotalCPUs > 0 && used+cpus > pol.MaxTotalCPUs {
		return &PolicyError{fmt.Sprintf("at most %d CPUs across %s; %d in use, this asks for %d",
			pol.MaxTotalCPUs, scope, used, cpus)}
	}
	return nil
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
