package cli

import "testing"

func TestJobKind(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"python", "fit.py", "--all"}, "fit"},
		{[]string{"python3 -u scripts/run_model.py --n 5"}, "run_model"},
		{[]string{"./run.sh"}, "run"},
		{[]string{"bash", "jobs/make plots.sh"}, "make_plots"},
		{[]string{"uv", "run", "train.py"}, "train"},
		{[]string{"python", "-m", "pkg.cli"}, "pkg"},
		{[]string{"echo hi"}, "echo"},
	} {
		if got := jobKind(tc.argv); got != tc.want {
			t.Errorf("jobKind(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestRemoteDirExpr(t *testing.T) {
	for in, want := range map[string]string{
		"projects/demo": `"$HOME"/projects/demo`,
		"~/demo":        `"$HOME"/demo`,
		"/scratch/demo": `/scratch/demo`,
		"my dir":        `"$HOME"/'my dir'`,
	} {
		if got := remoteDirExpr(in); got != want {
			t.Errorf("remoteDirExpr(%q) = %s, want %s", in, got, want)
		}
	}
}
