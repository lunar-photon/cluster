package slurm

import (
	"strings"
	"testing"
)

// Synthetic output in the shape `scontrol -o` and `sacctmgr -nP` produce.
const sampleInfo = `##ME
alice
##NODES
NodeName=cn1 Arch=x86_64 CoresPerSocket=16  CPUAlloc=0 CPUEfctv=32 CPUTot=32 CPULoad=0.01 AvailableFeatures=(null) ActiveFeatures=(null) Gres=(null) NodeAddr=cn1 OS=Linux 5.14.0 #1 SMP PREEMPT_DYNAMIC Tue Jan 1 UTC 2026  RealMemory=257000 AllocMem=0 FreeMem=250000 Sockets=2 Boards=1 State=IDLE ThreadsPerCore=1 Partitions=batch,long  CfgTRES=cpu=32,mem=257000M AllocTRES=
NodeName=cn2 Arch=x86_64 CoresPerSocket=16  CPUAlloc=20 CPUEfctv=32 CPUTot=32 AvailableFeatures=(null) Gres=(null) RealMemory=257000 AllocMem=0 FreeMem=100000 Sockets=2 State=MIXED ThreadsPerCore=1 Partitions=batch,long  AllocTRES=cpu=20
NodeName=cn3 Arch=x86_64 CoresPerSocket=16  CPUAlloc=32 CPUEfctv=32 CPUTot=32 AvailableFeatures=(null) Gres=(null) RealMemory=257000 AllocMem=0 FreeMem=5000 Sockets=2 State=ALLOCATED ThreadsPerCore=1 Partitions=batch,long  AllocTRES=cpu=32
NodeName=cn4 Arch=x86_64 CoresPerSocket=16  CPUAlloc=0 CPUEfctv=32 CPUTot=32 AvailableFeatures=(null) Gres=(null) RealMemory=257000 AllocMem=0 FreeMem=N/A Sockets=2 State=DOWN+NOT_RESPONDING ThreadsPerCore=1 Partitions=batch,long  Reason=Not responding [slurm@2026-01-01T00:00:00]
NodeName=cn5 Arch=x86_64 CoresPerSocket=16  CPUAlloc=0 CPUEfctv=32 CPUTot=32 AvailableFeatures=(null) Gres=(null) RealMemory=257000 AllocMem=0 FreeMem=240000 Sockets=2 State=IDLE+DRAIN ThreadsPerCore=1 Partitions=batch,long  Reason=maintenance
NodeName=g1 Arch=x86_64 CoresPerSocket=12  CPUAlloc=6 CPUEfctv=24 CPUTot=24 AvailableFeatures=a100 Gres=gpu:a100:4(S:0-1) RealMemory=515000 AllocMem=0 FreeMem=400000 Sockets=2 State=MIXED ThreadsPerCore=2 Partitions=gpu  AllocTRES=cpu=6,gres/gpu=1
##PARTS
PartitionName=batch AllowGroups=ALL AllowAccounts=ALL AllowQos=ALL Default=YES QoS=N/A DefaultTime=NONE MaxNodes=UNLIMITED MaxTime=1-00:00:00 MaxCPUsPerNode=UNLIMITED Nodes=cn[1-5] State=UP TotalCPUs=160 TotalNodes=5 DefMemPerNode=UNLIMITED MaxMemPerNode=UNLIMITED
PartitionName=long AllowGroups=ALL AllowAccounts=ALL AllowQos=ALL Default=NO MaxNodes=UNLIMITED MaxTime=7-00:00:00 Nodes=cn[1-5] State=UP TotalCPUs=160 TotalNodes=5
PartitionName=gpu AllowGroups=ALL AllowAccounts=ALL AllowQos=gpu_qos Default=NO MaxNodes=1 MaxTime=2-00:00:00 Nodes=g1 State=UP TotalCPUs=24 TotalNodes=1
PartitionName=staff AllowGroups=ALL AllowAccounts=admins AllowQos=ALL Default=NO MaxTime=UNLIMITED Nodes=cn1 State=UP TotalCPUs=32 TotalNodes=1
##ASSOC
lab||cpu_qos,gpu_qos|cpu_qos|||||
##QOS
normal||cpu=448|||||
cpu_qos||cpu=96,node=3|||||
gpu_qos|1-00:00:00|cpu=24,gres/gpu=2|||||
##MYJOBS
101|batch|RUNNING|cpu_qos|20|1|N/A
102|gpu|RUNNING|gpu_qos|6|1|gres:gpu:1
103|batch|PENDING|cpu_qos|32|1|N/A
`

func TestParseInfoAndSummarize(t *testing.T) {
	info := ParseInfo(sampleInfo)
	if info.User != "alice" || len(info.Nodes) != 6 || len(info.Partitions) != 4 {
		t.Fatalf("parsed user=%q nodes=%d parts=%d", info.User, len(info.Nodes), len(info.Partitions))
	}
	if info.MemTracked {
		t.Error("AllocMem is 0 on busy nodes, so memory is not tracked")
	}
	g1 := info.Nodes[5]
	if g1.GPUs != 4 || g1.GPUType != "a100" || g1.GPUAlloc != 1 || g1.ThreadsPerCore != 2 {
		t.Errorf("gpu node: %+v", g1)
	}
	if n := info.Nodes[3]; n.Usable() || n.FreeMemMB != -1 || !strings.HasPrefix(n.Reason, "Not responding") {
		t.Errorf("down node: %+v", n)
	}

	sums := map[string]Summary{}
	for _, s := range info.Summarize() {
		sums[s.Name] = s
	}
	b := sums["batch"]
	if b.Idle != 1 || b.Mixed != 1 || b.Allocated != 1 || b.Unavailable != 2 {
		t.Errorf("batch states idle=%d mixed=%d alloc=%d unavailable=%d", b.Idle, b.Mixed, b.Allocated, b.Unavailable)
	}
	if b.CPUsTotal != 160 || b.CPUsFree != 32+12 {
		t.Errorf("batch cpus total=%d free=%d", b.CPUsTotal, b.CPUsFree)
	}
	if b.BestFreeCPUs != 32 || b.BestFreeNode != "cn1" {
		t.Errorf("best free: %d on %s", b.BestFreeCPUs, b.BestFreeNode)
	}
	if len(b.Shapes) != 1 || b.Shapes[0].Count != 5 ||
		b.Shapes[0].String() != "32 cores (2 sockets × 16), 251 GB RAM" {
		t.Errorf("shapes: %+v %q", b.Shapes, b.Shapes[0].String())
	}
	if !b.Allowed {
		t.Errorf("batch should be allowed: %s", b.Why)
	}
	if CompressHosts(b.DownNodes) != "cn[4-5]" {
		t.Errorf("down nodes: %v", b.DownNodes)
	}

	g := sums["gpu"]
	if g.GPUsTotal != 4 || g.GPUsFree != 3 || !g.Allowed || len(g.AllowedVia) != 1 || g.AllowedVia[0] != "gpu_qos" {
		t.Errorf("gpu summary: %+v", g)
	}
	if got := g.Shapes[0].String(); got != "24 cores (2 sockets × 12, 2 threads/core), 503 GB RAM, 4 a100 GPUs" {
		t.Errorf("gpu shape %q", got)
	}
	if s := sums["staff"]; s.Allowed || s.Why == "" {
		t.Errorf("staff should be denied: %+v", s.Why)
	}

	if q := info.QOS["cpu_qos"]; q.MaxTRESPerUserParsed["cpu"] != 96 || q.MaxTRESPerUserParsed["node"] != 3 {
		t.Errorf("qos limits: %+v", q)
	}
	u := info.Usage()
	if u["cpu_qos"].CPUs != 20 || u["gpu_qos"].GPUs != 1 {
		t.Errorf("usage (running only): %+v", u)
	}
	if info.DefaultQOS() != "cpu_qos" {
		t.Errorf("default qos %q", info.DefaultQOS())
	}
}

func TestCompressHosts(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"cn6", "cn7", "cn8", "cn10", "gpu1"}, "cn[6-8,10],gpu1"},
		{[]string{"n01", "n02", "n03"}, "n[01-03]"},
		{[]string{"login"}, "login"},
		{nil, ""},
	} {
		if got := CompressHosts(tc.in); got != tc.want {
			t.Errorf("CompressHosts(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseTRES(t *testing.T) {
	m := ParseTRES("cpu=168,node=6,mem=128451M,gres/gpu=2,billing=28")
	if m["cpu"] != 168 || m["node"] != 6 || m["mem"] != 128451 || m["gres/gpu"] != 2 {
		t.Errorf("%v", m)
	}
	if ParseTRES("mem=2G")["mem"] != 2048 {
		t.Error("mem=2G")
	}
}

func TestAvailableToAndDetail(t *testing.T) {
	info := ParseInfo(sampleInfo)
	sums := map[string]Summary{}
	for _, s := range info.Summarize() {
		sums[s.Name] = s
	}
	// batch: 44 CPUs free; cpu_qos allows 96 - 20 used = 76, so free resources limit it.
	if a := info.AvailableTo(sums["batch"]); a.CPUs != 44 || a.Nodes != 2 || a.LimitedBy != "free resources" {
		t.Errorf("batch available: %+v", a)
	}
	// gpu: 3 GPUs free, but gpu_qos allows 2 - 1 used = 1.
	if a := info.AvailableTo(sums["gpu"]); a.GPUs != 1 || a.LimitedBy != "QOS gpu_qos" {
		t.Errorf("gpu available: %+v", a)
	}
	text := strings.Join(info.Detail(sums["batch"]), "\n")
	for _, want := range []string{
		"Partition batch (default) — up",
		"jobs run under QOS cpu_qos (≤96 CPUs, ≤3 nodes)",
		"Nodes: 5 × 32 cores (2 sockets × 16), 251 GB RAM.",
		"Now: 1 idle (cn1), 1 partly used, 1 full, 2 unavailable (cn[4-5]).",
		"one node can give a job up to 32 cores right now (cn1)",
		"Slurm doesn't reserve memory per job here",
		"Available to you now: 44 CPUs on up to 2 nodes (limited by free resources).",
		"cn4: Not responding",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("detail missing %q:\n%s", want, text)
		}
	}
	if got := info.AccountText(); got != "account lab · QOS cpu_qos, gpu_qos (default cpu_qos)" {
		t.Errorf("account text %q", got)
	}
}
