package slurm

import (
	"fmt"
	"strings"
)

// QOSFor is the QOS a job in this partition would run under: the configured
// one if the partition accepts it, else one the partition requires, else the
// user's default.
func (info *Info) QOSFor(s Summary) string {
	if len(s.AllowedVia) > 0 {
		if contains(s.AllowedVia, info.PreferredQOS) {
			return info.PreferredQOS
		}
		return s.AllowedVia[0]
	}
	if info.PreferredQOS != "" && info.hasQOS(info.PreferredQOS) {
		return info.PreferredQOS
	}
	return info.DefaultQOS()
}

func (info *Info) hasQOS(q string) bool {
	for _, a := range info.Assocs {
		if contains(a.QOS, q) {
			return true
		}
	}
	return len(info.Assocs) == 0
}

// Plural is "1 GPU", "3 GPUs".
func Plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// LimitText describes a QOS's per-user limits, e.g. "≤168 CPUs, ≤6 nodes".
func (info *Info) LimitText(qos string) string {
	q, ok := info.QOS[qos]
	if !ok {
		return ""
	}
	var parts []string
	l := q.MaxTRESPerUserParsed
	if v := l["cpu"]; v > 0 {
		parts = append(parts, fmt.Sprintf("≤%d CPUs", v))
	}
	if v := l["node"]; v > 0 {
		parts = append(parts, fmt.Sprintf("≤%d nodes", v))
	}
	if v := l["gres/gpu"]; v > 0 {
		parts = append(parts, fmt.Sprintf("≤%d GPUs", v))
	}
	if v := l["mem"]; v > 0 {
		parts = append(parts, "≤"+FormatMem(v)+" RAM")
	}
	if q.MaxJobsPerUser != "" {
		parts = append(parts, "≤"+q.MaxJobsPerUser+" running jobs")
	}
	if q.MaxWall != "" {
		parts = append(parts, "≤"+q.MaxWall+" per job")
	}
	if len(parts) == 0 {
		return "no per-user limits"
	}
	return strings.Join(parts, ", ")
}

// Available is what the user could start in a partition right now.
type Available struct {
	CPUs, Nodes, GPUs int
	// LimitedBy says which constraint is tighter: "free resources" or the QOS.
	LimitedBy string
}

// AvailableTo computes min(free in partition, remaining QOS allowance).
func (info *Info) AvailableTo(s Summary) Available {
	a := Available{CPUs: s.CPUsFree, Nodes: s.Idle + s.Mixed, GPUs: s.GPUsFree, LimitedBy: "free resources"}
	qos := info.QOSFor(s)
	q, ok := info.QOS[qos]
	if !ok {
		return a
	}
	used := info.Usage()[qos]
	limit := q.MaxTRESPerUserParsed
	if v := limit["cpu"]; v > 0 && v-used.CPUs < a.CPUs {
		a.CPUs, a.LimitedBy = max(0, v-used.CPUs), "QOS "+qos
	}
	if v := limit["node"]; v > 0 && v-used.Nodes < a.Nodes {
		a.Nodes, a.LimitedBy = max(0, v-used.Nodes), "QOS "+qos
	}
	if v := limit["gres/gpu"]; v > 0 && v-used.GPUs < a.GPUs {
		a.GPUs, a.LimitedBy = max(0, v-used.GPUs), "QOS "+qos
	}
	return a
}

// AccessText is a short access column value: "yes", "yes (gpu_qos)", "no".
func (s Summary) AccessText() string {
	switch {
	case !s.Allowed:
		return "no"
	case len(s.AllowedVia) > 0:
		return "yes (" + strings.Join(s.AllowedVia, ",") + ")"
	}
	return "yes"
}

// ShapeText summarises node hardware, e.g. "45 × 28 cores (2 sockets × 14), 125 GB RAM".
func (s Summary) ShapeText() string {
	var parts []string
	for _, sh := range s.Shapes {
		parts = append(parts, fmt.Sprintf("%d × %s", sh.Count, sh))
	}
	return strings.Join(parts, "; ")
}

// StateText is "idle/partly used/full/unavailable" node counts.
func (s Summary) StateText() string {
	return fmt.Sprintf("%d/%d/%d/%d", s.Idle, s.Mixed, s.Allocated, s.Unavailable)
}

// Unlimited renders Slurm's UNLIMITED/NONE/empty as a word.
func Unlimited(v string) string {
	switch v {
	case "", "UNLIMITED", "NONE", "N/A":
		return "unlimited"
	}
	return v
}

// Detail is a multi-line description of one partition for the given user.
func (info *Info) Detail(s Summary) []string {
	var lines []string
	title := "Partition " + s.Name
	if s.Default {
		title += " (default)"
	}
	lines = append(lines, title+" — "+strings.ToLower(s.State))

	if s.Allowed {
		qos := info.QOSFor(s)
		access := "You can use it"
		if qos != "" {
			access += "; jobs run under QOS " + qos
			if lt := info.LimitText(qos); lt != "" {
				access += " (" + lt + ")"
			}
		}
		lines = append(lines, access+".")
	} else {
		lines = append(lines, "You can't use it: "+s.Why+".")
	}

	limit := "Time limit " + Unlimited(s.MaxTime) + " per job"
	if d := s.DefaultTime; d != "" && d != "NONE" && d != s.MaxTime {
		limit += " (" + d + " unless you ask for more)"
	}
	lines = append(lines, limit+"; max nodes per job: "+Unlimited(s.MaxNodes)+".")

	for _, sh := range s.Shapes {
		lines = append(lines, fmt.Sprintf("Nodes: %d × %s.", sh.Count, sh))
	}

	state := fmt.Sprintf("Now: %d idle", s.Idle)
	if len(s.IdleNodes) > 0 {
		state += " (" + CompressHosts(s.IdleNodes) + ")"
	}
	state += fmt.Sprintf(", %d partly used, %d full, %d unavailable", s.Mixed, s.Allocated, s.Unavailable)
	if len(s.DownNodes) > 0 {
		state += " (" + CompressHosts(s.DownNodes) + ")"
	}
	lines = append(lines, state+".")

	cpu := fmt.Sprintf("CPUs: %d of %d free", s.CPUsFree, s.CPUsTotal)
	if s.BestFreeCPUs > 0 {
		cpu += fmt.Sprintf("; one node can give a job up to %d cores right now (%s)", s.BestFreeCPUs, s.BestFreeNode)
	}
	lines = append(lines, cpu+".")
	if s.GPUsTotal > 0 {
		lines = append(lines, fmt.Sprintf("GPUs: %d of %d free.", s.GPUsFree, s.GPUsTotal))
	}
	mem := "Memory: " + FormatMem(maxShapeMem(s.Shapes)) + " per node"
	if s.OSFreeMemMB > 0 {
		mem += "; " + FormatMem(s.OSFreeMemMB) + " free on the nodes that have free cores"
		if !info.MemTracked {
			mem += " (as the OS reports it; Slurm doesn't reserve memory per job here)"
		}
	}
	lines = append(lines, mem+".")

	if s.Allowed {
		a := info.AvailableTo(s)
		avail := fmt.Sprintf("Available to you now: %s on up to %s", Plural(a.CPUs, "CPU"), Plural(a.Nodes, "node"))
		if s.GPUsTotal > 0 {
			avail += ", " + Plural(a.GPUs, "GPU")
		}
		lines = append(lines, avail+" (limited by "+a.LimitedBy+").")
	}

	var reasons []string
	for _, n := range s.Nodes {
		if !n.Usable() && n.Reason != "" {
			reasons = append(reasons, n.Name+": "+n.Reason)
		}
	}
	if len(reasons) > 0 {
		if len(reasons) > 4 {
			reasons = append(reasons[:4], fmt.Sprintf("and %d more", len(reasons)-4))
		}
		lines = append(lines, "Unavailable: "+strings.Join(reasons, "; ")+".")
	}
	return lines
}

func maxShapeMem(shapes []Shape) int {
	m := 0
	for _, s := range shapes {
		m = max(m, s.MemMB)
	}
	return m
}

// AccountText summarises the user's accounts and QOS for a header line.
func (info *Info) AccountText() string {
	accounts, qos := []string{}, []string{}
	seenA, seenQ := map[string]bool{}, map[string]bool{}
	for _, a := range info.Assocs {
		if !seenA[a.Account] {
			seenA[a.Account] = true
			accounts = append(accounts, a.Account)
		}
		for _, q := range a.QOS {
			if !seenQ[q] {
				seenQ[q] = true
				qos = append(qos, q)
			}
		}
	}
	if len(accounts) == 0 {
		return "no accounting information"
	}
	s := "account " + strings.Join(accounts, ", ")
	if len(qos) > 0 {
		s += " · QOS " + strings.Join(qos, ", ")
		if d := info.DefaultQOS(); d != "" {
			s += " (default " + d + ")"
		}
	}
	return s
}

// UsageText lists each QOS's limits with what the user's running jobs use.
func (info *Info) UsageText() []string {
	usage := info.Usage()
	var lines []string
	seen := map[string]bool{}
	for _, a := range info.Assocs {
		for _, q := range a.QOS {
			if seen[q] {
				continue
			}
			seen[q] = true
			u := usage[q]
			line := q + ": " + info.LimitText(q)
			line += "; you're using " + Plural(u.CPUs, "CPU") + " on " + Plural(u.Nodes, "node")
			if u.GPUs > 0 {
				line += ", " + Plural(u.GPUs, "GPU")
			}
			lines = append(lines, line)
		}
	}
	return lines
}
