package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLegacy(t *testing.T) {
	src := `# cluster personal configuration
HOST="padmanabha.example.edu"
SLURM_PARTITION="medium"
SLURM_ACCOUNT="lab"
SLURM_QOS="6node_qos"
SLURM_CPUS=4"
SLURM_MEM="8G"   # trailing comment
SLURM_TIME='04:00:00'
GPU_PARTITION="gpu"
GPU_QOS="gpu_qos"
MOUNT_POINT="$HOME/Cluster"
`
	c := ParseLegacy(src)
	if c.Host != "padmanabha.example.edu" || c.Slurm.Account != "lab" || c.Slurm.QOS != "6node_qos" {
		t.Fatalf("basic fields: %+v", c)
	}
	if c.Slurm.CPUs != 4 {
		t.Errorf("CPUs from the old TUI's unbalanced quote: got %d", c.Slurm.CPUs)
	}
	if c.Slurm.Mem != "8G" || c.Slurm.Time != "04:00:00" {
		t.Errorf("quoting/comments: mem=%q time=%q", c.Slurm.Mem, c.Slurm.Time)
	}
	gpu := c.Profiles["gpu"]
	if gpu.Partition != "gpu" || gpu.QOS != "gpu_qos" || gpu.GPUs != 1 {
		t.Errorf("gpu profile: %+v", gpu)
	}
	if ExpandHome(c.MountPoint) == c.MountPoint {
		t.Errorf("$HOME not expandable: %q", c.MountPoint)
	}
}

func TestProfileMerge(t *testing.T) {
	c := Defaults()
	c.Slurm.Partition = "medium"
	c.Slurm.Account = "lab"
	c.Slurm.QOS = "cpu_qos"
	c.Profiles["gpu"] = Slurm{Partition: "gpu", GPUs: 1, QOS: "gpu_qos"}

	gpu, err := c.Profile("gpu")
	if err != nil {
		t.Fatal(err)
	}
	if gpu.Partition != "gpu" || gpu.QOS != "gpu_qos" || gpu.Account != "lab" || gpu.CPUs != 4 {
		t.Errorf("gpu should override partition/qos and inherit the rest: %+v", gpu)
	}
	cpu, _ := c.Profile("")
	if cpu.Partition != "medium" || cpu.QOS != "cpu_qos" {
		t.Errorf("default profile: %+v", cpu)
	}
	if _, err := c.Profile("nope"); err == nil {
		t.Error("unknown profile should fail")
	}
}

func TestLoadPrecedenceAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("CLUSTER_CONFIG", "")
	t.Setenv("CLUSTER_HOST", "")
	t.Setenv("CLUSTER_PROFILE", "")

	// Legacy file only.
	legacy := filepath.Join(dir, "cluster", "config")
	os.MkdirAll(filepath.Dir(legacy), 0o755)
	os.WriteFile(legacy, []byte("HOST=\"old.example\"\nSLURM_CPUS=8\n"), 0o644)
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Legacy || c.Host != "old.example" || c.Slurm.CPUs != 8 || c.Slurm.Mem != "" || c.Slurm.Partition != "" {
		t.Fatalf("legacy load: %+v", c)
	}

	// Migrating writes TOML that then takes precedence.
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c2.Legacy || c2.Host != "old.example" || c2.Slurm.CPUs != 8 {
		t.Fatalf("toml load: %+v", c2)
	}
	if p, _ := c2.Profile("gpu"); p.GPUs != 1 {
		t.Errorf("gpu profile lost in round trip: %+v", c2.Profiles)
	}

	t.Setenv("CLUSTER_HOST", "env.example")
	c3, _ := Load("")
	if c3.Host != "env.example" {
		t.Errorf("CLUSTER_HOST should override file: %q", c3.Host)
	}
}

func TestUserProfilesReplaceDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte(`
host = "h"
default_profile = "big"
[profiles.big]
cpus = 32
`), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Profiles["gpu"]; ok {
		t.Error("built-in gpu profile should not leak into a user's [profiles]")
	}
	p, err := c.Profile("")
	if err != nil || p.CPUs != 32 || p.Partition != "" {
		t.Errorf("big profile: %+v %v", p, err)
	}
}

func TestDefaultsNameNoSiteSpecifics(t *testing.T) {
	c := Defaults()
	if c.Host != "" || c.Slurm.Partition != "" || c.Slurm.Account != "" || c.Slurm.QOS != "" ||
		c.Slurm.Mem != "" || c.Slurm.Time != "" || c.Profiles["gpu"].Partition != "" {
		t.Errorf("defaults must not assume anything about a site: %+v %+v", c.Slurm, c.Profiles)
	}
	if ParseLegacy("GPU_QOS=x\n").Profiles["gpu"].Partition != "" {
		t.Error("legacy import must not invent a GPU partition name")
	}
}
