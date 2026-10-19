package slurm

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Node is one compute node as `scontrol show node` reports it.
type Node struct {
	Name           string
	Partitions     []string
	State          string // e.g. "MIXED", "IDLE+DRAIN", "DOWN+NOT_RESPONDING"
	CPUs, CPUAlloc int
	Sockets        int
	CoresPerSocket int
	ThreadsPerCore int
	MemMB          int // RealMemory
	AllocMemMB     int
	FreeMemMB      int // OS-reported free memory; -1 when unknown
	GPUs, GPUAlloc int
	GPUType        string
	Features       string
	Reason         string
}

// Usable reports whether Slurm will start new work on the node.
func (n *Node) Usable() bool {
	for _, flag := range strings.Split(n.State, "+") {
		switch flag {
		case "DOWN", "DRAIN", "DRAINED", "DRAINING", "FAIL", "FAILING", "NOT_RESPONDING",
			"MAINT", "RESERVED", "POWERED_DOWN", "POWERING_DOWN", "UNKNOWN", "FUTURE", "REBOOT_ISSUED", "INVALID_REG":
			return false
		}
	}
	return true
}

// BaseState is the state without flags: IDLE, MIXED, ALLOCATED, DOWN, ...
func (n *Node) BaseState() string { s, _, _ := strings.Cut(n.State, "+"); return s }

func (n *Node) FreeCPUs() int {
	if !n.Usable() {
		return 0
	}
	return max(0, n.CPUs-n.CPUAlloc)
}

func (n *Node) FreeGPUs() int {
	if !n.Usable() {
		return 0
	}
	return max(0, n.GPUs-n.GPUAlloc)
}

// PartitionInfo is one partition as `scontrol show partition` reports it.
type PartitionInfo struct {
	Name           string
	Default        bool
	State          string
	MaxTime        string
	DefaultTime    string
	MaxNodes       string
	MaxCPUsPerNode string
	MaxMemPerNode  string
	DefMemPerNode  string
	DefMemPerCPU   string
	AllowAccounts  []string // ["ALL"] when unrestricted
	AllowQOS       []string
	TotalNodes     int
	TotalCPUs      int
	Nodes          []*Node
}

// Assoc is one of the user's Slurm associations.
type Assoc struct {
	Account, Partition string
	QOS                []string
	DefaultQOS         string
	GrpTRES, MaxTRES   string
	MaxJobs, MaxSubmit string
	MaxWall            string
}

// QOS holds the limits of one quality of service.
type QOS struct {
	Name                 string
	MaxWall              string
	MaxTRESPerUser       string
	MaxJobsPerUser       string
	MaxSubmitPerUser     string
	MaxTRES, GrpTRES     string
	MaxTRESPerUserParsed map[string]int
	MaxTRESPerJobParsed  map[string]int
}

// MyJob is one of the user's pending or running jobs, for usage totals.
type MyJob struct {
	ID, Partition, State, QOS string
	CPUs, Nodes, GPUs         int
}

// Info is a snapshot of the cluster from the user's point of view.
type Info struct {
	User       string
	Partitions []*PartitionInfo
	Nodes      []*Node
	Assocs     []Assoc
	QOS        map[string]QOS
	MyJobs     []MyJob
	// MemTracked is false when Slurm doesn't reserve memory per job (every
	// busy node reports AllocMem=0), so "free memory" only means OS-free.
	MemTracked bool
	// PreferredQOS is the QOS this tool requests (from the config), used
	// where a partition accepts it.
	PreferredQOS string
}

const infoScript = `echo '##ME'; echo "$USER"
echo '##NODES'; scontrol show node -o 2>/dev/null
echo '##PARTS'; scontrol show partition -o 2>/dev/null
echo '##ASSOC'; sacctmgr -nP show assoc user="$USER" format=account,partition,qos,defaultqos,grptres,maxtres,maxjobs,maxsubmit,maxwall 2>/dev/null
echo '##QOS'; sacctmgr -nP show qos format=name,maxwall,maxtrespu,maxjobspu,maxsubmitpu,maxtres,grptres 2>/dev/null
echo '##MYJOBS'; squeue -h -u "$USER" -t R,PD,CF -o '%i|%P|%T|%q|%C|%D|%b' 2>/dev/null
true`

// ClusterInfo gathers nodes, partitions, the user's limits and usage in one
// round trip. sacctmgr may be unavailable (no accounting database); limits
// are then simply empty.
func (c *Client) ClusterInfo(ctx context.Context) (*Info, error) {
	out, err := c.R.Output(ctx, "bash -s", strings.NewReader(infoScript))
	if err != nil {
		return nil, err
	}
	return ParseInfo(out), nil
}

// NodeInfo describes one node, e.g. the one a job landed on.
func (c *Client) NodeInfo(ctx context.Context, name string) (*Node, error) {
	out, err := c.R.Run(ctx, "scontrol", "show", "node", "-o", name)
	if err != nil {
		return nil, err
	}
	n := parseNode(keyValues(strings.TrimSpace(out)))
	if n.Name == "" {
		return nil, fmt.Errorf("no node %q", name)
	}
	return n, nil
}

// Shape is the node's hardware description.
func (n *Node) Shape() Shape {
	return Shape{CPUs: n.CPUs, Sockets: n.Sockets, CoresPerSocket: n.CoresPerSocket,
		ThreadsPerCore: n.ThreadsPerCore, MemMB: n.MemMB, GPUs: n.GPUs, GPUType: n.GPUType}
}

// ParseInfo parses the output of infoScript.
func ParseInfo(out string) *Info {
	sections := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "##") {
			cur = strings.TrimPrefix(line, "##")
			continue
		}
		if strings.TrimSpace(line) != "" {
			sections[cur] = append(sections[cur], line)
		}
	}
	info := &Info{QOS: map[string]QOS{}}
	if me := sections["ME"]; len(me) > 0 {
		info.User = strings.TrimSpace(me[0])
	}
	byPart := map[string][]*Node{}
	for _, line := range sections["NODES"] {
		n := parseNode(keyValues(line))
		if n.Name == "" {
			continue
		}
		info.Nodes = append(info.Nodes, n)
		for _, p := range n.Partitions {
			byPart[p] = append(byPart[p], n)
		}
		if n.CPUAlloc > 0 && n.AllocMemMB > 0 {
			info.MemTracked = true
		}
	}
	for _, line := range sections["PARTS"] {
		p := parsePartition(keyValues(line))
		if p.Name == "" {
			continue
		}
		p.Nodes = byPart[p.Name]
		info.Partitions = append(info.Partitions, p)
	}
	for _, line := range sections["ASSOC"] {
		f := strings.Split(line, "|")
		if len(f) < 9 {
			continue
		}
		info.Assocs = append(info.Assocs, Assoc{
			Account: f[0], Partition: f[1], QOS: splitList(f[2]), DefaultQOS: f[3],
			GrpTRES: f[4], MaxTRES: f[5], MaxJobs: f[6], MaxSubmit: f[7], MaxWall: f[8],
		})
	}
	for _, line := range sections["QOS"] {
		f := strings.Split(line, "|")
		if len(f) < 7 {
			continue
		}
		info.QOS[f[0]] = QOS{
			Name: f[0], MaxWall: f[1], MaxTRESPerUser: f[2], MaxJobsPerUser: f[3], MaxSubmitPerUser: f[4],
			MaxTRES: f[5], GrpTRES: f[6],
			MaxTRESPerUserParsed: ParseTRES(f[2]), MaxTRESPerJobParsed: ParseTRES(f[5]),
		}
	}
	for _, line := range sections["MYJOBS"] {
		f := strings.Split(line, "|")
		if len(f) < 7 {
			continue
		}
		cpus, _ := strconv.Atoi(f[4])
		nodes, _ := strconv.Atoi(f[5])
		info.MyJobs = append(info.MyJobs, MyJob{
			ID: f[0], Partition: f[1], State: f[2], QOS: f[3], CPUs: cpus, Nodes: nodes, GPUs: gresGPUs(f[6]) * max(1, nodes),
		})
	}
	return info
}

var kvKey = regexp.MustCompile(`(?:^|\s)([A-Za-z][A-Za-z0-9_/]*)=`)

// keyValues splits a `scontrol -o` line. Values may contain spaces (OS=...,
// Reason=...), so each value runs up to the next " Key=".
func keyValues(line string) map[string]string {
	m := map[string]string{}
	locs := kvKey.FindAllStringSubmatchIndex(line, -1)
	for i, loc := range locs {
		key := line[loc[2]:loc[3]]
		end := len(line)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		m[key] = strings.TrimSpace(line[loc[1]:end])
	}
	return m
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

var gpuGres = regexp.MustCompile(`gpu(?::([A-Za-z0-9_.-]+))?:(\d+)`)

// gresGPUs counts GPUs in a gres string like "gpu:tesla:2(S:0-1)" or "gres:gpu:1".
func gresGPUs(s string) int {
	total := 0
	for _, m := range gpuGres.FindAllStringSubmatch(s, -1) {
		total += atoi(m[2])
	}
	return total
}

func parseNode(kv map[string]string) *Node {
	n := &Node{
		Name:           kv["NodeName"],
		State:          kv["State"],
		CPUs:           atoi(kv["CPUTot"]),
		CPUAlloc:       atoi(kv["CPUAlloc"]),
		Sockets:        atoi(kv["Sockets"]),
		CoresPerSocket: atoi(kv["CoresPerSocket"]),
		ThreadsPerCore: atoi(kv["ThreadsPerCore"]),
		MemMB:          atoi(kv["RealMemory"]),
		AllocMemMB:     atoi(kv["AllocMem"]),
		FreeMemMB:      -1,
		Reason:         kv["Reason"],
	}
	if eff := atoi(kv["CPUEfctv"]); eff > 0 {
		n.CPUs = eff
	}
	if v, err := strconv.Atoi(kv["FreeMem"]); err == nil {
		n.FreeMemMB = v
	}
	if p := kv["Partitions"]; p != "" && p != "(null)" {
		n.Partitions = strings.Split(p, ",")
	}
	if f := kv["AvailableFeatures"]; f != "(null)" {
		n.Features = f
	}
	if g := kv["Gres"]; g != "" && g != "(null)" {
		n.GPUs = gresGPUs(g)
		if m := gpuGres.FindStringSubmatch(g); m != nil && m[1] != "" {
			n.GPUType = m[1]
		}
	}
	n.GPUAlloc = ParseTRES(kv["AllocTRES"])["gres/gpu"]
	return n
}

func parsePartition(kv map[string]string) *PartitionInfo {
	return &PartitionInfo{
		Name:           kv["PartitionName"],
		Default:        kv["Default"] == "YES",
		State:          kv["State"],
		MaxTime:        kv["MaxTime"],
		DefaultTime:    kv["DefaultTime"],
		MaxNodes:       kv["MaxNodes"],
		MaxCPUsPerNode: kv["MaxCPUsPerNode"],
		MaxMemPerNode:  kv["MaxMemPerNode"],
		DefMemPerNode:  kv["DefMemPerNode"],
		DefMemPerCPU:   kv["DefMemPerCPU"],
		AllowAccounts:  splitList(kv["AllowAccounts"]),
		AllowQOS:       splitList(kv["AllowQos"]),
		TotalNodes:     atoi(kv["TotalNodes"]),
		TotalCPUs:      atoi(kv["TotalCPUs"]),
	}
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// ParseTRES turns "cpu=168,node=6,gres/gpu=2,mem=128451M" into a map. Memory
// is converted to MB.
func ParseTRES(s string) map[string]int {
	m := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if k == "mem" {
			m[k] = memMB(v)
			continue
		}
		m[k] = atoi(v)
	}
	return m
}

func memMB(v string) int {
	if v == "" {
		return 0
	}
	mult := 1.0
	switch v[len(v)-1] {
	case 'K':
		mult = 1.0 / 1024
	case 'M':
	case 'G':
		mult = 1024
	case 'T':
		mult = 1024 * 1024
	default:
		f, _ := strconv.ParseFloat(v, 64)
		return int(f)
	}
	f, _ := strconv.ParseFloat(v[:len(v)-1], 64)
	return int(f * mult)
}

// ---- summaries ----

// Shape groups nodes with identical hardware.
type Shape struct {
	CPUs, Sockets, CoresPerSocket, ThreadsPerCore int
	MemMB                                         int
	GPUs                                          int
	GPUType                                       string
	Count                                         int
}

func (s Shape) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d cores", s.CPUs)
	if s.Sockets > 0 && s.CoresPerSocket > 0 {
		fmt.Fprintf(&b, " (%d socket", s.Sockets)
		if s.Sockets != 1 {
			b.WriteString("s")
		}
		fmt.Fprintf(&b, " × %d", s.CoresPerSocket)
		if s.ThreadsPerCore > 1 {
			fmt.Fprintf(&b, ", %d threads/core", s.ThreadsPerCore)
		}
		b.WriteString(")")
	}
	fmt.Fprintf(&b, ", %s RAM", FormatMem(s.MemMB))
	if s.GPUs > 0 {
		t := ""
		if s.GPUType != "" {
			t = " " + s.GPUType
		}
		fmt.Fprintf(&b, ", %d%s GPU", s.GPUs, t)
		if s.GPUs != 1 {
			b.WriteString("s")
		}
	}
	return b.String()
}

// FormatMem renders MB as GB with sensible precision.
func FormatMem(mb int) string {
	gb := float64(mb) / 1024
	switch {
	case mb <= 0:
		return "0 GB"
	case gb >= 100:
		return fmt.Sprintf("%.0f GB", gb)
	default:
		return fmt.Sprintf("%.1f GB", gb)
	}
}

// Summary is what the dashboard shows for one partition.
type Summary struct {
	*PartitionInfo
	Shapes []Shape
	// Node counts by state.
	Idle, Mixed, Allocated, Unavailable int
	CPUsTotal, CPUsFree                 int
	GPUsTotal, GPUsFree                 int
	MemTotalMB                          int
	OSFreeMemMB                         int // OS-free memory on nodes with free cores
	// IdleNodes are fully free nodes; BestFreeCPUs is the most free cores on
	// any one usable node (what a single-node job could get right now).
	IdleNodes    []string
	BusyNodes    []string
	DownNodes    []string
	BestFreeCPUs int
	BestFreeNode string
	// Access for the current user.
	Allowed    bool
	AllowedVia []string // the user's QOS that this partition accepts
	Why        string   // why it is not allowed
}

// Summarize computes partition summaries from the user's point of view.
func (info *Info) Summarize() []Summary {
	myAccounts, myQOS := map[string]bool{}, map[string]bool{}
	for _, a := range info.Assocs {
		myAccounts[a.Account] = true
		for _, q := range a.QOS {
			myQOS[q] = true
		}
	}
	out := make([]Summary, 0, len(info.Partitions))
	for _, p := range info.Partitions {
		s := Summary{PartitionInfo: p}
		shapes := map[Shape]int{}
		for _, n := range p.Nodes {
			shape := Shape{CPUs: n.CPUs, Sockets: n.Sockets, CoresPerSocket: n.CoresPerSocket,
				ThreadsPerCore: n.ThreadsPerCore, MemMB: n.MemMB, GPUs: n.GPUs, GPUType: n.GPUType}
			shapes[shape]++
			s.CPUsTotal += n.CPUs
			s.GPUsTotal += n.GPUs
			s.MemTotalMB += n.MemMB
			if !n.Usable() {
				s.Unavailable++
				s.DownNodes = append(s.DownNodes, n.Name)
				continue
			}
			if n.FreeMemMB > 0 && n.FreeCPUs() > 0 {
				s.OSFreeMemMB += n.FreeMemMB // only where a new job could land
			}
			s.CPUsFree += n.FreeCPUs()
			s.GPUsFree += n.FreeGPUs()
			switch {
			case n.CPUAlloc == 0 && n.GPUAlloc == 0:
				s.Idle++
				s.IdleNodes = append(s.IdleNodes, n.Name)
			case n.FreeCPUs() > 0:
				s.Mixed++
				s.BusyNodes = append(s.BusyNodes, n.Name)
			default:
				s.Allocated++
				s.BusyNodes = append(s.BusyNodes, n.Name)
			}
			if f := n.FreeCPUs(); f > s.BestFreeCPUs {
				s.BestFreeCPUs, s.BestFreeNode = f, n.Name
			}
		}
		for shape, count := range shapes {
			shape.Count = count
			s.Shapes = append(s.Shapes, shape)
		}
		sort.Slice(s.Shapes, func(i, j int) bool { return s.Shapes[i].Count > s.Shapes[j].Count })

		s.Allowed, s.AllowedVia, s.Why = access(p, info.Assocs, myAccounts, myQOS)
		out = append(out, s)
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func access(p *PartitionInfo, assocs []Assoc, myAccounts, myQOS map[string]bool) (bool, []string, string) {
	if p.State != "" && p.State != "UP" {
		return false, nil, "partition is " + strings.ToLower(p.State)
	}
	if len(assocs) == 0 {
		return true, nil, "" // no accounting data: assume allowed
	}
	acctOK := len(p.AllowAccounts) == 0 || contains(p.AllowAccounts, "ALL")
	for a := range myAccounts {
		if contains(p.AllowAccounts, a) {
			acctOK = true
		}
	}
	if !acctOK {
		return false, nil, "your accounts are not allowed here"
	}
	// Partition-specific associations restrict which partitions the user may use.
	restricted, partOK := false, false
	for _, a := range assocs {
		if a.Partition != "" {
			restricted = true
			if a.Partition == p.Name {
				partOK = true
			}
		}
	}
	if restricted && !partOK {
		return false, nil, "no association for this partition"
	}
	if len(p.AllowQOS) == 0 || contains(p.AllowQOS, "ALL") {
		return true, nil, ""
	}
	var via []string
	for _, q := range p.AllowQOS {
		if myQOS[q] {
			via = append(via, q)
		}
	}
	if len(via) == 0 {
		return false, nil, "needs QOS " + strings.Join(p.AllowQOS, " or ")
	}
	return true, via, ""
}

// DefaultQOS is the QOS the user's jobs get when none is requested.
func (info *Info) DefaultQOS() string {
	for _, a := range info.Assocs {
		if a.DefaultQOS != "" {
			return a.DefaultQOS
		}
	}
	return ""
}

// Usage sums the user's running jobs per QOS.
func (info *Info) Usage() map[string]MyJob {
	u := map[string]MyJob{}
	for _, j := range info.MyJobs {
		if j.State != "RUNNING" {
			continue
		}
		x := u[j.QOS]
		x.QOS = j.QOS
		x.CPUs += j.CPUs
		x.Nodes += j.Nodes
		x.GPUs += j.GPUs
		u[j.QOS] = x
	}
	return u
}

// CompressHosts turns ["cn6","cn7","cn8","gpu1"] into "cn[6-8],gpu1".
func CompressHosts(names []string) string {
	type key struct {
		prefix string
		width  int
	}
	groups := map[key][]int{}
	var plain []string
	re := regexp.MustCompile(`^(.*?)(\d+)$`)
	for _, n := range names {
		m := re.FindStringSubmatch(n)
		if m == nil {
			plain = append(plain, n)
			continue
		}
		width := 0
		if strings.HasPrefix(m[2], "0") && len(m[2]) > 1 {
			width = len(m[2])
		}
		groups[key{m[1], width}] = append(groups[key{m[1], width}], atoi(m[2]))
	}
	var parts []string
	for k, nums := range groups {
		sort.Ints(nums)
		var ranges []string
		for i := 0; i < len(nums); {
			j := i
			for j+1 < len(nums) && nums[j+1] == nums[j]+1 {
				j++
			}
			f := func(n int) string { return fmt.Sprintf("%0*d", k.width, n) }
			if i == j {
				ranges = append(ranges, f(nums[i]))
			} else {
				ranges = append(ranges, f(nums[i])+"-"+f(nums[j]))
			}
			i = j + 1
		}
		if len(nums) == 1 {
			parts = append(parts, k.prefix+ranges[0])
		} else {
			parts = append(parts, k.prefix+"["+strings.Join(ranges, ",")+"]")
		}
	}
	sort.Strings(parts)
	sort.Strings(plain)
	return strings.Join(append(parts, plain...), ",")
}

// partitionMissing matches scontrol's message for an unknown partition.
var partitionMissing = regexp.MustCompile(`Partition \S+ not found|Invalid partition`)

// PartitionLimits reads one partition (the default one when name is empty).
// found is false when the cluster has no such partition.
func (c *Client) PartitionLimits(ctx context.Context, name string) (p *PartitionInfo, found bool, err error) {
	if name == "" {
		if name, err = c.DefaultPartition(ctx); err != nil || name == "" {
			return nil, false, err
		}
	}
	out, err := c.R.Run(ctx, "scontrol", "show", "partition", "-o", name)
	if err != nil {
		if partitionMissing.MatchString(err.Error() + " " + out) { // scontrol prints it on stdout
			return nil, false, nil
		}
		return nil, false, err
	}
	p = parsePartition(keyValues(strings.TrimSpace(out)))
	return p, p.Name != "", nil
}
