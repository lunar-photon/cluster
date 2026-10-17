package config

import (
	"strconv"
	"strings"
)

// ParseLegacy reads the KEY="VALUE" file used by the bash version without
// executing it. Unknown keys and malformed lines are ignored.
func ParseLegacy(src string) Config {
	var c Config
	var gpu Slurm
	var port int
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = unquoteLegacy(strings.TrimSpace(v))
		switch k {
		case "HOST":
			c.Host = v
		case "PORT":
			port, _ = strconv.Atoi(v)
		case "SLURM_PARTITION":
			c.Slurm.Partition = v
		case "SLURM_ACCOUNT":
			c.Slurm.Account = v
		case "SLURM_QOS":
			c.Slurm.QOS = v
		case "SLURM_CPUS":
			c.Slurm.CPUs, _ = strconv.Atoi(v)
		case "SLURM_MEM":
			c.Slurm.Mem = v
		case "SLURM_TIME":
			c.Slurm.Time = v
		case "GPU_PARTITION":
			gpu.Partition = v
		case "GPU_QOS":
			gpu.QOS = v
		case "MOUNT_POINT":
			for _, h := range []string{"$HOME", "${HOME}"} {
				if strings.HasPrefix(v, h) {
					v = "~" + v[len(h):]
				}
			}
			c.MountPoint = v
		case "NOTEBOOK_DIR":
			c.Jupyter.Dir = v
		}
	}
	c.Jupyter.Port = port
	gpu.GPUs = 1
	if gpu.Partition == "" {
		gpu.Partition = "gpu"
	}
	c.Profiles = map[string]Slurm{"cpu": {}, "gpu": gpu}
	return c
}

// unquoteLegacy strips one level of matching quotes and any trailing comment
// outside them. It also tolerates the unbalanced `4"` the old TUI wrote.
func unquoteLegacy(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : end+1]
		}
		return strings.Trim(v[1:], `"'`)
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.Trim(strings.TrimSpace(v), `"'`)
}
