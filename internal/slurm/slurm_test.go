package slurm

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lunar-photon/cluster/internal/config"
)

func TestRequestArgs(t *testing.T) {
	r := Request{Name: "cluster:terminal", Slurm: config.Slurm{
		Partition: "gpu", Account: "lab", QOS: "gpu_qos", CPUs: 8, Mem: "16G", Time: "02:00:00", GPUs: 2,
		Extra: []string{"--constraint=a100"},
	}}
	want := []string{
		"--job-name=cluster:terminal", "--partition=gpu", "--account=lab", "--qos=gpu_qos",
		"--cpus-per-task=8", "--mem=16G", "--time=02:00:00", "--gres=gpu:2", "--constraint=a100",
	}
	if got := r.Args(); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
	if got := (Request{}).Args(); len(got) != 0 {
		t.Errorf("empty request should add no flags: %v", got)
	}
}

func TestParseSqueueText(t *testing.T) {
	row := strings.Join([]string{"123", "gpu", "my job|with pipe", "me", "PENDING", "0:00", "4:00:00", "1", "(Resources)", "8", "16G", "gres:gpu:2"}, sep)
	jobs := parseSqueueText(row + "\n\ngarbage\n")
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	j := jobs[0]
	if j.ID != "123" || j.Name != "my job|with pipe" || j.State != "PENDING" || j.NodeList != "(Resources)" || j.TimeLimit != "4:00:00" ||
		j.CPUs != "8" || j.Mem != "16G" || j.GPUs() != 2 {
		t.Errorf("%+v", j)
	}
}
