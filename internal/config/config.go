// Package config loads and saves cluster settings.
//
// Settings live in a TOML file (default ~/.config/cluster/config.toml) holding
// connection settings, base Slurm defaults, and named profiles that override
// those defaults. The file is parsed, never executed.
//
// Precedence, highest first: command-line flags, environment
// (CLUSTER_HOST, CLUSTER_PROFILE), the selected profile, the [slurm] table,
// built-in defaults.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Slurm holds allocation parameters. A zero value in a profile means
// "inherit from the [slurm] table".
type Slurm struct {
	Partition string `toml:"partition,omitempty"`
	Account   string `toml:"account,omitempty"`
	QOS       string `toml:"qos,omitempty"`
	CPUs      int    `toml:"cpus,omitempty,omitzero"`
	Mem       string `toml:"mem,omitempty"`
	Time      string `toml:"time,omitempty"`
	GPUs      int    `toml:"gpus,omitempty,omitzero"`
	// Extra is appended verbatim to the sbatch/srun arguments, e.g. ["--constraint=a100"].
	Extra []string `toml:"extra,omitempty"`
}

// Merge returns base with every non-zero field of over applied on top.
func (base Slurm) Merge(over Slurm) Slurm {
	out := base
	if over.Partition != "" {
		out.Partition = over.Partition
	}
	if over.Account != "" {
		out.Account = over.Account
	}
	if over.QOS != "" {
		out.QOS = over.QOS
	}
	if over.CPUs != 0 {
		out.CPUs = over.CPUs
	}
	if over.Mem != "" {
		out.Mem = over.Mem
	}
	if over.Time != "" {
		out.Time = over.Time
	}
	if over.GPUs != 0 {
		out.GPUs = over.GPUs
	}
	if len(over.Extra) > 0 {
		out.Extra = append(append([]string{}, base.Extra...), over.Extra...)
	}
	return out
}

type Jupyter struct {
	// Port is the first local port tried for the tunnel.
	Port int `toml:"port,omitempty,omitzero"`
	// Dir is the remote notebook directory (default: remote home).
	Dir string `toml:"dir,omitempty"`
	// Setup is shell run on the remote side before starting Jupyter,
	// e.g. "module load anaconda3 && conda activate ml".
	Setup string `toml:"setup,omitempty"`
	// Profile is the profile used by `cluster jupyter` when no mode flag is given.
	Profile string `toml:"profile,omitempty"`
}

type Sync struct {
	Exclude []string `toml:"exclude,omitempty"`
}

type Config struct {
	Host           string           `toml:"host"`
	MountPoint     string           `toml:"mount_point,omitempty"`
	RemoteDir      string           `toml:"remote_dir,omitempty"`
	DefaultProfile string           `toml:"default_profile,omitempty"`
	Slurm          Slurm            `toml:"slurm"`
	Profiles       map[string]Slurm `toml:"profiles,omitempty"`
	Jupyter        Jupyter          `toml:"jupyter"`
	Sync           Sync             `toml:"sync"`
	// JobPrefix starts the name of every job cluster submits.
	JobPrefix string `toml:"job_prefix,omitempty"`
	// Policy holds usage rules that apply everywhere; a project's
	// .cluster.toml can add its own.
	// A pointer so an unset policy writes no empty [policy] table.
	Policy *Policy `toml:"policy,omitempty"`

	// Project is the .cluster.toml found from the working directory, if any.
	Project *Project `toml:"-"`

	// Path is where the config was loaded from (or will be saved to).
	Path string `toml:"-"`
	// Legacy is true when the values came from the old shell-style file.
	Legacy bool `toml:"-"`
	// Source is the file the values were read from; empty for built-in defaults.
	Source string `toml:"-"`
}

// DefaultJobPrefix names jobs when the config sets no job_prefix.
const DefaultJobPrefix = "cluster:"

// JobPrefix in effect: the project's, else the user's, else the default;
// replaced by the policy's required prefix if it doesn't already start with it.
func (c Config) EffectiveJobPrefix() string {
	prefix := c.JobPrefix
	if c.Project != nil && c.Project.JobPrefix != "" {
		prefix = c.Project.JobPrefix
	}
	if prefix == "" {
		prefix = DefaultJobPrefix
	}
	if req := c.EffectivePolicy().JobPrefix; req != "" && !strings.HasPrefix(prefix, req) {
		prefix = req
	}
	return prefix
}

// JobName is the Slurm job name for a kind of job, e.g. "cluster:terminal".
func (c Config) JobName(kind string) string {
	return c.EffectiveJobPrefix() + kind
}

var DefaultSyncExcludes = []string{
	".git/", "__pycache__/", "*.pyc", ".venv/", "venv/", "*.log", ".ipynb_checkpoints/",
}

// Defaults returns the built-in configuration. It names no partition, QOS,
// account, memory or time limit: those are site-specific, so unset fields
// fall back to the cluster's own defaults, and `cluster init` fills them in
// from what the cluster reports.
func Defaults() Config {
	return Config{
		MountPoint:     "~/Cluster",
		DefaultProfile: "cpu",
		Slurm: Slurm{
			CPUs: 4,
		},
		Profiles: map[string]Slurm{
			"cpu": {},
			"gpu": {GPUs: 1},
		},
		Jupyter:   Jupyter{Port: 8888},
		Sync:      Sync{Exclude: DefaultSyncExcludes},
		JobPrefix: DefaultJobPrefix,
	}
}

func configHome() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

// DefaultPath is the TOML config location, overridable with CLUSTER_CONFIG.
func DefaultPath() string {
	if p := os.Getenv("CLUSTER_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(configHome(), "cluster", "config.toml")
}

// LegacyPaths are the shell-style files written by the bash version.
func LegacyPaths() []string {
	return []string{
		filepath.Join(configHome(), "cluster", "config"),
		filepath.Join(configHome(), "cluster-jupyter", "config"),
	}
}

// Load reads the config from path (DefaultPath when empty). If no TOML file
// exists, it falls back to a legacy shell-style file, then to defaults.
// Environment overrides are applied last.
func Load(path string) (Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	cfg := Defaults()
	cfg.Path = path

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Decode onto a copy whose maps are empty so that a user's [profiles]
		// table replaces, rather than merges with, the built-in profiles.
		var file Config
		if _, err := toml.Decode(string(data), &file); err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
		cfg = overlay(cfg, file)
		cfg.Source = path
	case errors.Is(err, os.ErrNotExist):
		for _, lp := range LegacyPaths() {
			if legacy, err := os.ReadFile(lp); err == nil {
				cfg = overlay(cfg, ParseLegacy(string(legacy)))
				cfg.Legacy = true
				cfg.Source = lp
				break
			}
		}
	default:
		return cfg, err
	}

	if os.Getenv("CLUSTER_NO_PROJECT") == "" {
		wd, _ := os.Getwd()
		proj, err := FindProject(wd)
		if err != nil {
			return cfg, err
		}
		cfg.Project = proj
	}

	if h := os.Getenv("CLUSTER_HOST"); h != "" {
		cfg.Host = h
	}
	if p := os.Getenv("CLUSTER_PROFILE"); p != "" {
		cfg.DefaultProfile = p
	}
	return cfg, nil
}

// overlay applies every set field of file onto base.
func overlay(base, file Config) Config {
	out := base
	if file.Host != "" {
		out.Host = file.Host
	}
	if file.JobPrefix != "" {
		out.JobPrefix = file.JobPrefix
	}
	if file.Policy != nil {
		merged := out.Policy.value().merge(*file.Policy)
		out.Policy = &merged
	}
	if file.MountPoint != "" {
		out.MountPoint = file.MountPoint
	}
	if file.RemoteDir != "" {
		out.RemoteDir = file.RemoteDir
	}
	if file.DefaultProfile != "" {
		out.DefaultProfile = file.DefaultProfile
	}
	out.Slurm = base.Slurm.Merge(file.Slurm)
	if file.Profiles != nil {
		out.Profiles = file.Profiles
	}
	if file.Jupyter.Port != 0 {
		out.Jupyter.Port = file.Jupyter.Port
	}
	if file.Jupyter.Dir != "" {
		out.Jupyter.Dir = file.Jupyter.Dir
	}
	if file.Jupyter.Setup != "" {
		out.Jupyter.Setup = file.Jupyter.Setup
	}
	if file.Jupyter.Profile != "" {
		out.Jupyter.Profile = file.Jupyter.Profile
	}
	if file.Sync.Exclude != nil {
		out.Sync.Exclude = file.Sync.Exclude
	}
	return out
}

// Profile resolves a named profile on top of the [slurm] defaults. An empty
// name selects DefaultProfile.
// A project's [slurm] defaults apply between the user's [slurm] and the
// profile's own values.
func (c Config) Profile(name string) (Slurm, error) {
	base := c.Slurm
	if c.Project != nil {
		base = base.Merge(c.Project.Slurm)
	}
	if name == "" {
		name = c.DefaultProfile
	}
	if name == "" {
		return base, nil
	}
	p, ok := c.Profiles[name]
	if !ok {
		return Slurm{}, fmt.Errorf("unknown profile %q (have: %s)", name, strings.Join(c.ProfileNames(), ", "))
	}
	return base.Merge(p), nil
}

func (c Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ExpandHome replaces a leading ~ or $HOME with the local home directory.
func ExpandHome(p string) string {
	home, _ := os.UserHomeDir()
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "$HOME"):
		return home + p[len("$HOME"):]
	case strings.HasPrefix(p, "${HOME}"):
		return home + p[len("${HOME}"):]
	}
	return p
}

// ShortPath shows a path with the home directory as ~.
func ShortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

// LocalMountPoint is MountPoint with ~ expanded.
func (c Config) LocalMountPoint() string { return ExpandHome(c.MountPoint) }

// Encode renders the config as TOML.
func (c Config) Encode() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# cluster configuration — see `cluster config --help`\n\n")
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Save writes the config atomically to c.Path (DefaultPath when empty).
func (c Config) Save() error {
	path := c.Path
	if path == "" {
		path = DefaultPath()
	}
	data, err := c.Encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
