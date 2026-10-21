package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// ProjectFile is the name of a project's own settings file. cluster looks
// for it in the current directory and its parents.
const ProjectFile = ".cluster.toml"

// Policy is a set of usage rules. Commands that submit work check them
// first and refuse with a message naming the rule.
type Policy struct {
	// JobPrefix: every job name must start with this.
	JobPrefix string `toml:"job_prefix,omitempty"`
	// Partitions that may be used; empty allows all.
	Partitions []string `toml:"partitions,omitempty"`
	// MaxJobs: active (pending or running) jobs at once. When JobPrefix is
	// set, only jobs whose names start with it count.
	MaxJobs int `toml:"max_jobs,omitempty,omitzero"`
	// MaxCPUs per job.
	MaxCPUs int `toml:"max_cpus,omitempty,omitzero"`
	// MaxTotalCPUs across the counted active jobs, including the new one.
	MaxTotalCPUs int `toml:"max_total_cpus,omitempty,omitzero"`
}

// value is the policy, or no rules when it isn't set.
func (p *Policy) value() Policy {
	if p == nil {
		return Policy{}
	}
	return *p
}

// Empty reports whether the policy sets no rules.
func (p Policy) Empty() bool {
	return p.JobPrefix == "" && len(p.Partitions) == 0 && p.MaxJobs == 0 && p.MaxCPUs == 0 && p.MaxTotalCPUs == 0
}

// merge returns p with every rule other sets taking its place.
func (p Policy) merge(other Policy) Policy {
	if other.JobPrefix != "" {
		p.JobPrefix = other.JobPrefix
	}
	if other.Partitions != nil {
		p.Partitions = other.Partitions
	}
	if other.MaxJobs != 0 {
		p.MaxJobs = other.MaxJobs
	}
	if other.MaxCPUs != 0 {
		p.MaxCPUs = other.MaxCPUs
	}
	if other.MaxTotalCPUs != 0 {
		p.MaxTotalCPUs = other.MaxTotalCPUs
	}
	return p
}

// Lines describes the rules for people, e.g. "at most 3 jobs at once".
func (p Policy) Lines() []string {
	var out []string
	if p.JobPrefix != "" {
		out = append(out, "job names start with "+p.JobPrefix)
	}
	if len(p.Partitions) > 0 {
		out = append(out, "partitions: "+strings.Join(p.Partitions, ", "))
	}
	if p.MaxJobs > 0 {
		out = append(out, fmt.Sprintf("at most %d jobs at once", p.MaxJobs))
	}
	if p.MaxCPUs > 0 {
		out = append(out, fmt.Sprintf("at most %d CPUs per job", p.MaxCPUs))
	}
	if p.MaxTotalCPUs > 0 {
		out = append(out, fmt.Sprintf("at most %d CPUs across running and queued jobs", p.MaxTotalCPUs))
	}
	return out
}

// Project holds a project's .cluster.toml. Its values apply on top of the
// user's config while working in the project, but are never written back
// into the user's config.
type Project struct {
	Path string `toml:"-"`
	// JobPrefix names this project's jobs.
	JobPrefix string `toml:"job_prefix,omitempty"`
	// RemotePath is where the project lives on the cluster, relative to the
	// remote home unless absolute. sync and submit use it.
	RemotePath string `toml:"remote_path,omitempty"`
	Policy     Policy `toml:"policy"`
	// Slurm defaults for this project, between the user's [slurm] and flags.
	Slurm Slurm `toml:"slurm"`
	Sync  Sync  `toml:"sync"`
}

// Dir is the project's root directory.
func (p *Project) Dir() string { return filepath.Dir(p.Path) }

// FindProject looks for .cluster.toml in dir and its parents, stopping at
// the home directory or the filesystem root. It returns nil if none.
func FindProject(dir string) (*Project, error) {
	home, _ := os.UserHomeDir()
	dir, _ = filepath.Abs(dir)
	for {
		path := filepath.Join(dir, ProjectFile)
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			p := &Project{Path: path}
			if _, err := toml.Decode(string(data), p); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			return p, nil
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
		parent := filepath.Dir(dir)
		if parent == dir || dir == home {
			return nil, nil
		}
		dir = parent
	}
}

// EffectivePolicy is the user's [policy] with the project's rules on top.
func (c Config) EffectivePolicy() Policy {
	p := c.Policy.value()
	if c.Project != nil {
		p = p.merge(c.Project.Policy)
	}
	return p
}

// SyncExcludes are the project's exclude patterns, else the user's.
func (c Config) SyncExcludes() []string {
	if c.Project != nil && c.Project.Sync.Exclude != nil {
		return c.Project.Sync.Exclude
	}
	return c.Sync.Exclude
}

// RemotePathFor maps a local directory to its place on the cluster: inside
// a project with remote_path, the same relative place under it; otherwise a
// directory of the same name in the remote home.
func (c Config) RemotePathFor(localAbs string) string {
	if c.Project != nil && c.Project.RemotePath != "" {
		if rel, err := filepath.Rel(c.Project.Dir(), localAbs); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			if rel == "." {
				return c.Project.RemotePath
			}
			return c.Project.RemotePath + "/" + filepath.ToSlash(rel)
		}
	}
	return filepath.Base(localAbs)
}
