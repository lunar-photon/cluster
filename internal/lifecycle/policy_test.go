package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/slurm"
)

func TestCheckPolicy(t *testing.T) {
	f := newFake(t)
	// One active job named cluster:terminal using 4 CPUs (see fakeSqueue).
	os.WriteFile(filepath.Join(f.dir, "job_700"), []byte("RUNNING"), 0o644)
	req := func(name, part string, cpus int) slurm.Request {
		return slurm.Request{Name: name, Slurm: config.Slurm{Partition: part, CPUs: cpus}}
	}
	ctx := context.Background()
	for _, tc := range []struct {
		pol  config.Policy
		req  slurm.Request
		want string // substring of the refusal; "" means allowed
	}{
		{config.Policy{}, req("anything", "x", 64), ""},
		{config.Policy{JobPrefix: "pj_"}, req("cluster:exec", "batch", 1), `job names must start with "pj_"`},
		{config.Policy{MaxCPUs: 2}, req("cluster:exec", "batch", 4), "at most 2 CPUs per job; this asks for 4"},
		{config.Policy{Partitions: []string{"batch", "short"}}, req("cluster:exec", "gpu", 1), "partition gpu isn't allowed (allowed: batch, short)"},
		{config.Policy{Partitions: []string{"batch"}}, req("cluster:exec", "batch", 1), ""},
		{config.Policy{MaxJobs: 1}, req("cluster:exec", "batch", 1), "at most 1 jobs at once; 1 active (1 running, 0 pending)"},
		{config.Policy{MaxJobs: 2}, req("cluster:exec", "batch", 1), ""},
		{config.Policy{MaxTotalCPUs: 6}, req("cluster:exec", "batch", 4), "at most 6 CPUs across jobs; 4 in use, this asks for 4"},
		// Only jobs with the policy's prefix count: the cluster:* job doesn't.
		{config.Policy{JobPrefix: "pj_", MaxJobs: 1}, req("pj_exec", "batch", 1), ""},
	} {
		f.env.Cfg.Policy = tc.pol
		err := f.env.CheckPolicy(ctx, tc.req)
		var pe *PolicyError
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%+v: unexpected refusal %v", tc.pol, err)
		case tc.want != "" && (err == nil || !errors.As(err, &pe) || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%+v: got %v, want %q", tc.pol, err, tc.want)
		}
	}
}

func TestPolicyRefusesBeforeSubmitting(t *testing.T) {
	f := newFake(t, "0")
	f.env.Cfg.Policy = config.Policy{MaxCPUs: 2}
	err := f.env.StartShell(shellOpts()) // asks for 4 CPUs
	if err == nil || !strings.Contains(err.Error(), "policy: at most 2 CPUs per job") {
		t.Fatalf("got %v", err)
	}
	if f.read("sbatch_args") != "" {
		t.Error("nothing may be submitted when the policy refuses")
	}
}

// fakeScontrol knows one partition, "medium", with a 1-hour limit.
const fakeScontrol = `#!/usr/bin/env bash
if [[ "$1 $2" == "show partition" ]]; then
  if [[ "$4" == medium ]]; then echo "PartitionName=medium MaxTime=01:00:00 State=UP"; exit 0; fi
  echo "Partition $4 not found"; exit 1  # scontrol says this on stdout
fi
exit 1
`

func TestCheckRequestPartitionLimits(t *testing.T) {
	f := newFake(t)
	os.WriteFile(filepath.Join(f.dir, "bin", "scontrol"), []byte(fakeScontrol), 0o755)
	ctx := context.Background()
	req := func(part, tm string) slurm.Request {
		return slurm.Request{Name: "x", Slurm: config.Slurm{Partition: part, Time: tm, CPUs: 1}}
	}
	if err := f.env.CheckRequest(ctx, req("medium", "00:30:00")); err != nil {
		t.Errorf("within the limit: %v", err)
	}
	err := f.env.CheckRequest(ctx, req("medium", "04:00:00"))
	if err == nil || !strings.Contains(err.Error(), "partition medium allows at most 01:00:00 per job, but this asks for 04:00:00; pass -t 01:00:00 or less") {
		t.Errorf("over the limit: %v", err)
	}
	if err := f.env.CheckRequest(ctx, req("nosuch", "")); err == nil || !strings.Contains(err.Error(), `no partition "nosuch"`) {
		t.Errorf("unknown partition: %v", err)
	}
}
