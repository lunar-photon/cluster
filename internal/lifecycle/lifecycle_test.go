package lifecycle

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/remote"
	"github.com/lunar-photon/cluster/internal/session"
	"github.com/lunar-photon/cluster/internal/slurm"
)

// The fake cluster: ssh runs the remote command locally with fake Slurm tools
// first on PATH. Job state lives in files under $FAKE.
const fakeSSH = `#!/usr/bin/env bash
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o|-L) shift 2 ;;
    -G) echo "hostname 127.0.0.1"; echo "port $FAKE_PORT"; exit 0 ;;
    -*) shift ;;
    *) break ;;
  esac
done
shift # host
cd "$HOME" # like real ssh: remote commands start in the home directory
exec bash -c "$*"
`

const fakeSbatch = `#!/usr/bin/env bash
cat > "$FAKE/script"
echo "$*" > "$FAKE/sbatch_args"
echo RUNNING > "$FAKE/job_1001"
echo 1001
`

// squeue understands only what the client sends: --jobs=ID or -u.
const fakeSqueue = `#!/usr/bin/env bash
us=$'\x1f'
id=""
for a in "$@"; do case "$a" in --jobs=*) id="${a#--jobs=}" ;; esac; done
if [[ -n "$id" && ! -f "$FAKE/job_$id" ]]; then
  echo "slurm_load_jobs error: Invalid job id specified" >&2; exit 1
fi
for f in "$FAKE"/job_*; do
  [[ -f "$f" ]] || continue
  j="${f##*/job_}"
  [[ -n "$id" && "$j" != "$id" ]] && continue
  echo "$j${us}medium${us}cluster:shell${us}me${us}$(cat "$f")${us}0:05${us}4:00:00${us}1${us}node07"
done
`

const fakeScancel = `#!/usr/bin/env bash
for id in "$@"; do rm -f "$FAKE/job_$id"; echo "$id" >> "$FAKE/cancelled"; done
`

// srun pops its exit status from $FAKE/srun_codes, one per attach.
const fakeSrun = `#!/usr/bin/env bash
echo "$*" >> "$FAKE/srun_calls"
code=$(head -n1 "$FAKE/srun_codes"); sed -i 1d "$FAKE/srun_codes"
exit "${code:-0}"
`

// syncBuffer lets a test read output while a goroutine is still writing it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fake struct {
	dir string
	env *Env
	out *syncBuffer
}

func newFake(t *testing.T, srunCodes ...string) *fake {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	for name, body := range map[string]string{
		"ssh": fakeSSH, "sbatch": fakeSbatch, "squeue": fakeSqueue, "scancel": fakeScancel, "srun": fakeSrun,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "srun_codes"), []byte(strings.Join(srunCodes, "\n")+"\n"), 0o644)

	// Something listening, so Reachable succeeds after a "Wi-Fi drop".
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())

	t.Setenv("HOME", filepath.Join(dir, "home"))
	os.MkdirAll(filepath.Join(dir, "home"), 0o755)
	t.Setenv("FAKE", dir)
	t.Setenv("FAKE_PORT", port)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	r := remote.New("fakehost")
	r.SSH = filepath.Join(bin, "ssh")
	r.ControlPath = "" // no multiplexing in tests
	out := &syncBuffer{}
	r.Log = out
	e := &Env{
		Cfg:   config.Defaults(),
		R:     r,
		Slurm: slurm.New(r),
		Store: &session.Store{Path: filepath.Join(dir, "sessions.json")},
		Out:   out,
	}
	return &fake{dir: dir, env: e, out: out}
}

func (f *fake) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, name))
	return string(b)
}

func (f *fake) sessions(t *testing.T) []session.Session {
	l, err := f.env.Store.List()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func shellOpts() ShellOpts {
	return ShellOpts{
		Req:     slurm.Request{Slurm: config.Slurm{Partition: "medium", CPUs: 4}},
		Profile: "cpu", Persist: true, Overlap: true,
	}
}

func TestShellExitReleasesJob(t *testing.T) {
	f := newFake(t, "0")
	if err := f.env.StartShell(shellOpts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if !strings.Contains(f.read("sbatch_args"), "--job-name=cluster:shell") {
		t.Errorf("sbatch args: %s", f.read("sbatch_args"))
	}
	if !strings.Contains(f.read("script"), "tmux -L \"cluster-$SLURM_JOB_ID\" new-session -d") {
		t.Errorf("hold script should start tmux:\n%s", f.read("script"))
	}
	if calls := f.read("srun_calls"); !strings.Contains(calls, "--jobid=1001 --overlap --pty") {
		t.Errorf("srun calls: %s", calls)
	}
	if got := strings.TrimSpace(f.read("cancelled")); got != "1001" {
		t.Errorf("job should be cancelled exactly once, got %q\n%s", got, f.out)
	}
	if !strings.Contains(f.out.String(), "Released job 1001") {
		t.Errorf("release not confirmed:\n%s", f.out)
	}
	if l := f.sessions(t); len(l) != 0 {
		t.Errorf("session should be forgotten: %+v", l)
	}
}

func TestConnectionLossReattaches(t *testing.T) {
	// First attach: ssh "drops" (255). Second: user exits normally.
	f := newFake(t, "255", "0")
	if err := f.env.StartShell(shellOpts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if n := strings.Count(f.read("srun_calls"), "--jobid=1001"); n != 2 {
		t.Errorf("expected 2 attaches, got %d\n%s", n, f.out)
	}
	out := f.out.String()
	if !strings.Contains(out, "Connection lost") || !strings.Contains(out, "reattaching") {
		t.Errorf("missing reconnect messages:\n%s", out)
	}
	if strings.TrimSpace(f.read("cancelled")) != "1001" {
		t.Errorf("job should be released after the final exit:\n%s", out)
	}
}

func TestDetachKeepsJob(t *testing.T) {
	f := newFake(t, "100")
	if err := f.env.StartShell(shellOpts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if f.read("cancelled") != "" {
		t.Error("detaching must not cancel the job")
	}
	l := f.sessions(t)
	if len(l) != 1 || l[0].JobID != "1001" {
		t.Fatalf("session should stay tracked: %+v", l)
	}

	// Reattach later, then exit.
	os.WriteFile(filepath.Join(f.dir, "srun_codes"), []byte("0\n"), 0o644)
	if err := f.env.Attach("1001", true, true); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if strings.TrimSpace(f.read("cancelled")) != "1001" || len(f.sessions(t)) != 0 {
		t.Errorf("attach+exit should release:\n%s", f.out)
	}
}

func TestJobEndedWhileDisconnected(t *testing.T) {
	f := newFake(t, "255")
	// Make the job vanish when the connection drops, as if it hit its time limit.
	srun := filepath.Join(f.dir, "bin", "srun")
	os.WriteFile(srun, []byte(strings.Replace(fakeSrun, `exit "${code:-0}"`, `rm -f "$FAKE/job_1001"; exit "${code:-0}"`, 1)), 0o755)

	if err := f.env.StartShell(shellOpts()); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if !strings.Contains(f.out.String(), "ended while disconnected") {
		t.Errorf("expected ended-while-disconnected:\n%s", f.out)
	}
	if n := strings.Count(f.read("srun_calls"), "--jobid"); n != 1 {
		t.Errorf("should not reattach to a dead job (attaches=%d)", n)
	}
	if len(f.sessions(t)) != 0 {
		t.Error("ended session should be forgotten")
	}
}

func TestGCForgetsEndedSessions(t *testing.T) {
	f := newFake(t)
	f.env.Store.Add(session.Session{JobID: "555", Kind: session.Shell})
	os.WriteFile(filepath.Join(f.dir, "job_1001"), []byte("RUNNING"), 0o644) // untracked cluster:* job
	if err := f.env.GC(false); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if len(f.sessions(t)) != 0 {
		t.Errorf("ended job 555 should be forgotten:\n%s", f.out)
	}
	if !strings.Contains(f.out.String(), "Untracked job 1001") {
		t.Errorf("untracked job not reported:\n%s", f.out)
	}
	if strings.Contains(f.read("cancelled"), "1001") {
		t.Errorf("untracked job must not be cancelled without the flag: %q", f.read("cancelled"))
	}
}

func TestAttachCommandQuoting(t *testing.T) {
	got := attachCommand("42", true, false)
	if strings.Contains(got, "--overlap") {
		t.Error("overlap should be omitted")
	}
	if !strings.HasPrefix(got, "srun --jobid=42 --pty bash -c '") {
		t.Errorf("unexpected: %s", got)
	}
}

func TestJupyterScriptKeepsTokenOutOfArgv(t *testing.T) {
	s := jupyterScript("module load x", "~/nb", "TOKEN123", "abcd")
	if !strings.Contains(s, "export JUPYTER_TOKEN=TOKEN123") {
		t.Error("token should be exported")
	}
	if strings.Contains(s, "--ServerApp.token") || strings.Contains(s, "--NotebookApp.token") {
		t.Error("token must not appear on the jupyter command line")
	}
	if !strings.Contains(s, `cd "$HOME"/nb`) {
		t.Errorf("~/ dir should expand remotely:\n%s", s)
	}
}
