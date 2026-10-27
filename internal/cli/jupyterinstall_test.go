package cli

import (
	"strings"
	"testing"
)

func TestBestPython(t *testing.T) {
	lines := []string{
		"|/usr/bin/python3|3.6",
		"python/3.10.6|/apps/python/3.10.6/bin/python3|3.10",
		"python/3.12.5|/apps/python/3.12.5/bin/python3|3.12",
		"garbage",
	}
	py, ok := bestPython(lines)
	if !ok || py.Module != "python/3.12.5" || py.Minor != 12 || py.load() != "module load python/3.12.5 && " {
		t.Errorf("%+v %v", py, ok)
	}
	// Same version without a module wins.
	py, _ = bestPython(append(lines, "|/usr/bin/python3.12|3.12"))
	if py.Module != "" || py.Path != "/usr/bin/python3.12" || py.load() != "" {
		t.Errorf("%+v", py)
	}
	if _, ok := bestPython([]string{"|/usr/bin/python3|3.6", "python/3.8|/x/python3|3.8"}); ok {
		t.Error("nothing 3.9+ means no candidate")
	}
}

func TestJupyterInstallScriptLoadsModule(t *testing.T) {
	s := jupyterInstallScript(pythonCandidate{"python/3.12.5", "/apps/py/bin/python3", 3, 12}, []string{"pip", "jupyterlab"})
	if !strings.Contains(s, "module load python/3.12.5\n") || !strings.Contains(s, "/apps/py/bin/python3 -m venv --without-pip") {
		t.Errorf("%s", s)
	}
}

func TestManylinuxTags(t *testing.T) {
	tags := strings.Join(manylinuxTags("2.28", "x86_64"), " ")
	for _, want := range []string{"manylinux_2_28_x86_64", "manylinux_2_17_x86_64", "manylinux_2_5_x86_64",
		"manylinux2014_x86_64", "manylinux2010_x86_64", "manylinux1_x86_64"} {
		if !strings.Contains(tags, want) {
			t.Errorf("missing %s in %s", want, tags)
		}
	}
	if strings.Contains(tags, "manylinux_2_29") {
		t.Error("tags newer than the cluster's glibc must not be allowed")
	}
	if tags := strings.Join(manylinuxTags("2.12", "aarch64"), " "); strings.Contains(tags, "manylinux2014") || !strings.Contains(tags, "manylinux2010_aarch64") {
		t.Errorf("old glibc: %s", tags)
	}
}

func TestPipDownloadArgs(t *testing.T) {
	args := strings.Join(pipDownloadArgs("/d", pythonCandidate{"", "/usr/bin/python3.11", 3, 11}, "2.28", "x86_64", []string{"pip", "jupyterlab"}), " ")
	for _, want := range []string{"--only-binary=:all:", "--python-version 3.11", "--abi cp311", "--abi abi3", "--platform manylinux_2_28_x86_64", "pip jupyterlab"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in %s", want, args)
		}
	}
}
