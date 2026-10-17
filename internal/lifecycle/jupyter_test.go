package lifecycle

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lunar-photon/cluster/internal/config"
	"github.com/lunar-photon/cluster/internal/slurm"
)

// fakeForward stands in for `ssh -N -L lport:host:rport`.
const fakeForward = `import socket, sys, threading
lp, host, rp = int(sys.argv[1]), sys.argv[2], int(sys.argv[3])
srv = socket.socket(); srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", lp)); srv.listen(16)
def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d: break
            b.sendall(d)
    except OSError: pass
    finally:
        for s in (a, b):
            try: s.close()
            except OSError: pass
while True:
    c, _ = srv.accept()
    u = socket.create_connection((host, rp))
    threading.Thread(target=pipe, args=(c, u), daemon=True).start()
    threading.Thread(target=pipe, args=(u, c), daemon=True).start()
`

func TestJupyterTunnelSurvivesDrop(t *testing.T) {
	f := newFake(t)
	home := os.Getenv("HOME")
	os.MkdirAll(filepath.Join(home, ".cluster"), 0o700)

	// "Jupyter": answers /api/status only with the right token.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var kernels atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token "+readToken(f) {
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/api/status":
			w.Write([]byte(`{}`))
		case "/api/kernels":
			w.Write([]byte("[" + strings.TrimSuffix(strings.Repeat(`{"id":"k"},`, int(kernels.Load())), ",") + "]"))
		default:
			w.WriteHeader(404)
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	jport := l.Addr().(*net.TCPAddr).Port

	bin := filepath.Join(f.dir, "bin")
	os.WriteFile(filepath.Join(bin, "fwd.py"), []byte(fakeForward), 0o644)
	// ssh -N -L 127.0.0.1:LP:HOST:RP → run the forwarder; otherwise as before.
	ssh := strings.Replace(fakeSSH, `while [[ $# -gt 0 ]]; do`, `for a in "$@"; do
  if [[ "$prev" == "-L" ]]; then IFS=: read -r _ lp h rp <<< "$a"; exec python3 "$(dirname "$0")/fwd.py" "$lp" "$h" "$rp"; fi
  prev="$a"
done
while [[ $# -gt 0 ]]; do`, 1)
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0o755)
	// sbatch publishes the connection file the real job script would write.
	os.WriteFile(filepath.Join(bin, "sbatch"), []byte(`#!/usr/bin/env bash
cat > "$FAKE/script"
echo RUNNING > "$FAKE/job_1001"
printf '{"host":"127.0.0.1","port":`+strconv.Itoa(jport)+`,"pid":1}' > "$HOME/.cluster/jupyter-1001.json"
echo 1001
`), 0o755)

	f.env.Cfg = config.Defaults()
	f.env.Cfg.Jupyter.Port = freePort(18888)

	done := make(chan error, 1)
	go func() {
		done <- f.env.Jupyter(JupyterOpts{Req: slurm.Request{}, Profile: "cpu"})
	}()

	waitFor(t, f, "JupyterLab: http://127.0.0.1:")
	sess, ok, _ := f.env.Store.Get("1001")
	if !ok || sess.TunnelPID == 0 || !strings.Contains(sess.URL, "token=") {
		t.Fatalf("session not recorded properly: %+v", sess)
	}

	// Drop the "network": kill the tunnel. It should come back by itself.
	syscall.Kill(sess.TunnelPID, syscall.SIGKILL)
	waitFor(t, f, "Tunnel restored: ")
	resp, err := http.Get(strings.Replace(sess.URL, "/lab?token=", "/api/status?x=", 1))
	if err == nil {
		resp.Body.Close()
	} else {
		t.Errorf("tunnel not usable after restore: %v", err)
	}

	// Ctrl+C with a kernel open asks first; answering no keeps Jupyter.
	kernels.Store(1)
	f.env.In = strings.NewReader("n\n")
	syscall.Kill(os.Getpid(), syscall.SIGINT)
	waitFor(t, f, "Jupyter keeps running.")
	waitFor(t, f, "Tunnel restored")
	if f.read("cancelled") != "" {
		t.Fatalf("answering no must not cancel the job\n%s", f.out)
	}

	// With no kernels, Ctrl+C stops Jupyter and releases the job.
	kernels.Store(0)
	syscall.Kill(os.Getpid(), syscall.SIGINT)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%v\n%s", err, f.out)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Jupyter did not stop on Ctrl+C\n%s", f.out)
	}
	if strings.TrimSpace(f.read("cancelled")) != "1001" {
		t.Errorf("job not cancelled:\n%s", f.out)
	}
	if _, err := os.Stat(filepath.Join(home, ".cluster", "jupyter-1001.json")); !os.IsNotExist(err) {
		t.Error("remote connection file should be removed")
	}
	if strings.Contains(f.read("script"), readToken(f)) && strings.Contains(f.read("script"), "--ServerApp.token") {
		t.Error("token leaked into argv")
	}
}

func readToken(f *fake) string {
	s := f.read("script")
	_, after, ok := strings.Cut(s, "export JUPYTER_TOKEN=")
	if !ok {
		return "\x00"
	}
	tok, _, _ := strings.Cut(after, "\n")
	return tok
}

func waitFor(t *testing.T, f *fake, text string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(f.out.String(), text) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q\n%s", text, f.out)
}

// Closing the terminal with kernels open leaves Jupyter running.
func TestJupyterTerminalCloseKeepsKernels(t *testing.T) {
	f := newFake(t)
	home := os.Getenv("HOME")
	os.MkdirAll(filepath.Join(home, ".cluster"), 0o700)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			w.Write([]byte(`{}`))
		case "/api/kernels":
			w.Write([]byte(`[{"id":"a"},{"id":"b"}]`))
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	jport := l.Addr().(*net.TCPAddr).Port
	bin := filepath.Join(f.dir, "bin")
	os.WriteFile(filepath.Join(bin, "fwd.py"), []byte(fakeForward), 0o644)
	ssh := strings.Replace(fakeSSH, `while [[ $# -gt 0 ]]; do`, `for a in "$@"; do
  if [[ "$prev" == "-L" ]]; then IFS=: read -r _ lp h rp <<< "$a"; exec python3 "$(dirname "$0")/fwd.py" "$lp" "$h" "$rp"; fi
  prev="$a"
done
while [[ $# -gt 0 ]]; do`, 1)
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0o755)
	os.WriteFile(filepath.Join(bin, "sbatch"), []byte(`#!/usr/bin/env bash
cat > "$FAKE/script"
echo RUNNING > "$FAKE/job_1001"
printf '{"host":"127.0.0.1","port":`+strconv.Itoa(jport)+`,"pid":1}' > "$HOME/.cluster/jupyter-1001.json"
echo 1001
`), 0o755)
	f.env.Cfg.Jupyter.Port = freePort(19888)

	done := make(chan error, 1)
	go func() { done <- f.env.Jupyter(JupyterOpts{Profile: "cpu"}) }()
	waitFor(t, f, "JupyterLab: http://127.0.0.1:")
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("did not return after SIGHUP\n%s", f.out)
	}
	if f.read("cancelled") != "" {
		t.Errorf("Jupyter with open kernels must be kept\n%s", f.out)
	}
	if !strings.Contains(f.out.String(), "2 Jupyter kernel(s) running, so Jupyter 1001 was kept") {
		t.Errorf("missing keep notice:\n%s", f.out)
	}
	if sess, ok, _ := f.env.Store.Get("1001"); !ok || sess.TunnelPID != 0 {
		t.Errorf("session should remain tracked with no tunnel: %+v", sess)
	}
}
