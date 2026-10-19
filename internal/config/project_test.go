package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectFileOverlay(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, ".config"))
	t.Setenv("CLUSTER_CONFIG", "")
	t.Setenv("CLUSTER_NO_PROJECT", "")
	user := filepath.Join(root, ".config", "cluster", "config.toml")
	os.MkdirAll(filepath.Dir(user), 0o755)
	os.WriteFile(user, []byte(`host = "h"
job_prefix = "me:"
[slurm]
partition = "batch"
cpus = 4
[policy]
max_cpus = 16
`), 0o644)

	proj := filepath.Join(root, "work", "proj")
	sub := filepath.Join(proj, "src", "deep")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(proj, ProjectFile), []byte(`
job_prefix = "pj_run_"
remote_path = "projects/pj"
[policy]
job_prefix = "pj_"
partitions = ["batch", "short"]
max_jobs = 3
[slurm]
cpus = 2
[sync]
exclude = ["data/"]
`), 0o644)

	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(sub) // found from a subdirectory

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Project == nil || c.Project.Dir() != proj || c.Project.RemotePath != "projects/pj" {
		t.Fatalf("project: %+v", c.Project)
	}
	pol := c.EffectivePolicy()
	if pol.JobPrefix != "pj_" || pol.MaxJobs != 3 || pol.MaxCPUs != 16 || len(pol.Partitions) != 2 {
		t.Errorf("policy merges user and project rules: %+v", pol)
	}
	if got := c.JobName("terminal"); got != "pj_run_terminal" {
		t.Errorf("job name %q", got)
	}
	if p, _ := c.Profile(""); p.CPUs != 2 || p.Partition != "batch" {
		t.Errorf("project slurm defaults: %+v", p)
	}
	if ex := c.SyncExcludes(); len(ex) != 1 || ex[0] != "data/" {
		t.Errorf("excludes %v", ex)
	}

	// Saving the user config never copies project values into it.
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(user)
	for _, leak := range []string{"pj_", "projects/pj", "data/", "max_jobs"} {
		if containsStr(string(data), leak) {
			t.Errorf("user config gained project value %q:\n%s", leak, data)
		}
	}

	// A policy prefix the configured prefix doesn't satisfy replaces it.
	c.Project.JobPrefix = ""
	if got := c.JobName("x"); got != "pj_x" {
		t.Errorf("policy prefix should win over %q: %q", c.JobPrefix, got)
	}

	// Outside the project there is none.
	os.Chdir(root)
	c2, _ := Load("")
	if c2.Project != nil || c2.EffectivePolicy().JobPrefix != "" || c2.JobName("t") != "me:t" {
		t.Errorf("outside project: %+v %q", c2.Project, c2.JobName("t"))
	}
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }
