package cli

import (
	"path"

	"github.com/spf13/cobra"

	"github.com/lunar-photon/cluster/internal/slurm"
)

// nameFlag adds --name, a glob on the job name such as 'proj_*'.
func nameFlag(cmd *cobra.Command, p *string) {
	cmd.Flags().StringVar(p, "name", "", "only jobs whose name matches this glob, e.g. 'proj_*'")
}

// nameMatches applies a --name glob; an empty pattern matches everything.
// A malformed pattern matches nothing rather than everything.
func nameMatches(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func filterJobs(jobs []slurm.Job, pattern string) []slurm.Job {
	if pattern == "" {
		return jobs
	}
	out := jobs[:0:0]
	for _, j := range jobs {
		if nameMatches(pattern, j.Name) {
			out = append(out, j)
		}
	}
	return out
}
