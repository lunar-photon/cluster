package tui

import (
	"testing"

	"github.com/lunar-photon/cluster/internal/config"
)

func TestRequestTextLabelsEveryValue(t *testing.T) {
	got := requestText(config.Slurm{Partition: "batch", CPUs: 4, Mem: "8G", Time: "04:00:00", QOS: "q1"}, 28, true)
	if got != "partition batch · 4 of 28 cores on one node · 8G memory · 04:00:00 time limit · QOS q1" {
		t.Errorf("%q", got)
	}
	if got := requestText(config.Slurm{CPUs: 1}, 0, true); got != "default partition · 1 core on one node · default memory · the partition's time limit" {
		t.Errorf("%q", got)
	}
	// A cluster that doesn't track memory per job: no memory in the line.
	if got := requestText(config.Slurm{Partition: "batch", CPUs: 4, Mem: "8G", Time: "04:00:00"}, 28, false); got != "partition batch · 4 of 28 cores on one node · 04:00:00 time limit" {
		t.Errorf("%q", got)
	}
}

func TestInheritedFields(t *testing.T) {
	cfg := config.Defaults()
	cfg.Slurm.Mem, cfg.Slurm.Time = "8G", "04:00:00"
	cfg.Profiles["gpu"] = config.Slurm{Partition: "g", GPUs: 1}
	if got := inheritedFields(cfg, "gpu", true); got != "Cores, memory and time limit come from the CPU settings" {
		t.Errorf("%q", got)
	}
	cfg.Profiles["gpu"] = config.Slurm{Partition: "g", GPUs: 1, CPUs: 8, Time: "1:00:00"}
	if got := inheritedFields(cfg, "gpu", true); got != "Memory comes from the CPU settings" {
		t.Errorf("%q", got)
	}
	if got := inheritedFields(cfg, "gpu", false); got != "" {
		t.Errorf("memory hidden, nothing else inherited: %q", got)
	}
	cfg.Profiles["gpu"] = config.Slurm{CPUs: 8, Mem: "1G", Time: "1:00:00"}
	if got := inheritedFields(cfg, "gpu", true); got != "" {
		t.Errorf("%q", got)
	}
}
