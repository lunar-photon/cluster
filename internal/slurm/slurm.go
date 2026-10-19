// Package slurm builds Slurm command lines and reads queue state.
package slurm

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/remote"
)

// Marker is the Slurm comment on every job this tool submits. `cluster gc`
// finds leftovers by it rather than by name, so jobs that merely share the
// configured name prefix (e.g. a project's own sbatch jobs) are never touched.
const Marker = "submitted-by-cluster"

// Request is a resource request; Args is the only place Slurm flags are built.
type Request struct {
	config.Slurm
	Name string
	// NoMarker leaves out the --comment marker (for tests of the flag set).
	NoMarker bool
}

func (r Request) Args() []string {
	var a []string
	if r.Name != "" {
		a = append(a, "--job-name="+r.Name)
	}
	if !r.NoMarker {
		a = append(a, "--comment="+Marker)
	}
	if r.Partition != "" {
		a = append(a, "--partition="+r.Partition)
	}
	if r.Account != "" {
		a = append(a, "--account="+r.Account)
	}
	if r.QOS != "" {
		a = append(a, "--qos="+r.QOS)
	}
	if r.CPUs > 0 {
		a = append(a, "--cpus-per-task="+strconv.Itoa(r.CPUs))
	}
	if r.Mem != "" {
		a = append(a, "--mem="+r.Mem)
	}
	if r.Time != "" {
		a = append(a, "--time="+r.Time)
	}
	if r.GPUs > 0 {
		a = append(a, "--gres=gpu:"+strconv.Itoa(r.GPUs))
	}
	return append(a, r.Extra...)
}

func (r Request) String() string { return strings.Join(r.Args(), " ") }

// Job is one squeue row.
type Job struct {
	ID        string
	Partition string
	Name      string
	User      string
	State     string
	Time      string
	TimeLimit string
	Nodes     string
	NodeList  string // nodelist when running, pending reason otherwise
	CPUs      string
	Mem       string // minimum memory requested, e.g. "8G"; "0" means the default
	GRES      string // e.g. "gres:gpu:1"; "N/A" when none
	Comment   string
}

// ByCluster reports whether this tool submitted the job.
func (j Job) ByCluster() bool { return j.Comment == Marker }

// GPUs is the number of GPUs the job requested per node.
func (j Job) GPUs() int { return gresGPUs(j.GRES) }

// Partition is one sinfo row.
type Partition struct {
	Name      string
	Avail     string
	TimeLimit string
	Nodes     string
	State     string
	NodeList  string
}

// Client queries Slurm through a remote client. Output is read in the
// classic -o format split on an ASCII unit separator rather than --json:
// several Slurm releases ignore --me/--jobs filters under --json.
type Client struct {
	R *remote.Client
}

func New(r *remote.Client) *Client { return &Client{R: r} }

// ---- squeue ----

const sep = "\x1f" // ASCII unit separator: cannot appear in job names typed at a shell

var squeueFormat = strings.Join([]string{"%i", "%P", "%j", "%u", "%T", "%M", "%l", "%D", "%R", "%C", "%m", "%b", "%k"}, sep)

// Jobs lists the current user's jobs. With ids, it lists only those jobs.
func (c *Client) Jobs(ctx context.Context, ids ...string) ([]Job, error) {
	cmd := "squeue --noheader -u \"$USER\" -o " + remote.Quote(squeueFormat)
	if len(ids) > 0 {
		cmd = "squeue --noheader --jobs=" + remote.Quote(strings.Join(ids, ",")) + " -o " + remote.Quote(squeueFormat)
	}
	out, err := c.R.Output(ctx, cmd, nil)
	if err != nil {
		// squeue --jobs fails with "Invalid job id" once a job has left the queue.
		if len(ids) > 0 && strings.Contains(err.Error(), "Invalid job id") {
			return nil, nil
		}
		return nil, err
	}
	return parseSqueueText(out), nil
}

func parseSqueueText(out string) []Job {
	var jobs []Job
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, sep)
		if len(f) < 9 {
			continue
		}
		j := Job{ID: f[0], Partition: f[1], Name: f[2], User: f[3], State: f[4], Time: f[5],
			TimeLimit: f[6], Nodes: f[7], NodeList: f[8]}
		if len(f) >= 12 {
			j.CPUs, j.Mem, j.GRES = f[9], f[10], f[11]
		}
		if len(f) >= 13 {
			j.Comment = f[12]
		}
		jobs = append(jobs, j)
	}
	return jobs
}

// ---- sinfo ----

var sinfoFormat = strings.Join([]string{"%P", "%a", "%l", "%D", "%T", "%N"}, sep)

// Partitions summarizes partitions and node states.
func (c *Client) Partitions(ctx context.Context) ([]Partition, error) {
	out, err := c.R.Output(ctx, "sinfo --noheader -o "+remote.Quote(sinfoFormat), nil)
	if err != nil {
		return nil, err
	}
	var parts []Partition
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, sep)
		if len(f) < 6 {
			continue
		}
		parts = append(parts, Partition{f[0], f[1], f[2], f[3], f[4], f[5]})
	}
	return parts, nil
}

// ---- job control ----

// Submit runs sbatch with the given script on stdin and returns the job ID.
// flags are extra sbatch options such as --output.
func (c *Client) Submit(ctx context.Context, req Request, script string, flags ...string) (string, error) {
	argv := append([]string{"sbatch", "--parsable"}, flags...)
	cmd := remote.Join(append(argv, req.Args()...)...)
	out, err := c.R.Output(ctx, cmd, strings.NewReader(script))
	if err != nil {
		return "", fmt.Errorf("sbatch: %w", err)
	}
	if c.R.DryRun {
		return "DRYRUN", nil
	}
	// --parsable prints "jobid" or "jobid;cluster"
	id, _, _ := strings.Cut(strings.TrimSpace(out), ";")
	if _, err := strconv.Atoi(id); err != nil {
		return "", fmt.Errorf("sbatch: unexpected output %q", out)
	}
	return id, nil
}

// Cancel runs scancel. Cancelling a job that already finished is not an error.
func (c *Client) Cancel(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := c.R.Run(ctx, append([]string{"scancel"}, ids...)...)
	if err != nil && strings.Contains(err.Error(), "Invalid job id") {
		return nil
	}
	return err
}

// Alive reports whether a job is still pending or running.
func (c *Client) Alive(ctx context.Context, id string) (Job, bool, error) {
	jobs, err := c.Jobs(ctx, id)
	if err != nil {
		return Job{}, false, err
	}
	for _, j := range jobs {
		if j.ID == id {
			return j, IsActive(j.State), nil
		}
	}
	return Job{}, false, nil
}

// IsActive is true for states in which the job still holds or awaits resources.
func IsActive(state string) bool {
	switch state {
	case "PENDING", "RUNNING", "CONFIGURING", "SUSPENDED", "REQUEUED", "RESIZING", "SIGNALING", "STAGE_OUT":
		return true
	}
	return false
}
