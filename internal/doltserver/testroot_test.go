package doltserver

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestSelectServersUnderRoots pins the difference between the startup sweep's
// selection and the exit-path sweep's: no deleted-cwd branch. SweepAbandoned
// TestRoots runs while sibling suites are mid-flight, so it may only reap
// servers inside roots it has already proved abandoned, and must ignore every
// signal that would let it reach outside them.
func TestSelectServersUnderRoots(t *testing.T) {
	cases := []struct {
		name       string
		candidates []serverCandidate
		roots      []string
		want       []int
	}{
		{
			name: "server under an abandoned root is reaped",
			candidates: []serverCandidate{
				{pid: 100, cmdline: "dolt sql-server -P 3308", cwd: "/tmp/beads-bd-tests-dead/x/.beads/dolt"},
			},
			roots: []string{"/tmp/beads-bd-tests-dead"},
			want:  []int{100},
		},
		{
			name: "deleted cwd OUTSIDE the roots is NOT reaped (unlike the exit-path sweep)",
			candidates: []serverCandidate{
				{pid: 101, cmdline: "dolt sql-server -P 3308", cwd: "/tmp/other-suite/.beads/dolt", cwdDeleted: true},
			},
			roots: []string{"/tmp/beads-bd-tests-dead"},
			want:  nil,
		},
		{
			name: "a concurrent run's live root is not among the roots, so its server is untouched",
			candidates: []serverCandidate{
				{pid: 102, cmdline: "dolt sql-server -P 3309", cwd: "/tmp/beads-bd-tests-live/.beads/dolt"},
			},
			roots: []string{"/tmp/beads-bd-tests-dead"},
			want:  nil,
		},
		{
			name: "production server is never reaped",
			candidates: []serverCandidate{
				{pid: 200, cmdline: "dolt sql-server -P 3307", cwd: "/home/dev/project/.beads/dolt"},
			},
			roots: []string{"/tmp/beads-bd-tests-dead"},
			want:  nil,
		},
		{
			name: "non-dolt process inside an abandoned root is ignored",
			candidates: []serverCandidate{
				{pid: 201, cmdline: "some-other-binary --flag", cwd: "/tmp/beads-bd-tests-dead/x"},
			},
			roots: []string{"/tmp/beads-bd-tests-dead"},
			want:  nil,
		},
		{
			name: "unknown cwd is left alone",
			candidates: []serverCandidate{
				{pid: 202, cmdline: "dolt sql-server -P 3308", cwd: ""},
			},
			roots: []string{"/tmp/beads-bd-tests-dead"},
			want:  nil,
		},
		{
			name: "no roots reaps nothing at all, deleted cwd included",
			candidates: []serverCandidate{
				{pid: 203, cmdline: "dolt sql-server -P 3308", cwd: "/tmp/anything", cwdDeleted: true},
			},
			roots: nil,
			want:  nil,
		},
		{
			name: "sibling path sharing a name prefix is not under the root",
			candidates: []serverCandidate{
				{pid: 204, cmdline: "dolt sql-server -P 3308", cwd: "/tmp/beads-bd-tests-dead2/.beads/dolt"},
			},
			roots: []string{"/tmp/beads-bd-tests-dead"},
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectServersUnderRoots(tc.candidates, tc.roots)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("selectServersUnderRoots() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAbandonedTestRoots checks the enumeration gate: only directories, never
// symlinks (SweepAbandonedTestRoots deletes what this returns), and only what
// the abandonment predicate accepts.
func TestAbandonedTestRoots(t *testing.T) {
	base := t.TempDir()

	dead := filepath.Join(base, "root-dead")
	live := filepath.Join(base, "root-live")
	for _, dir := range []string{dead, live} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A plain file matching the glob must not be returned.
	if err := os.WriteFile(filepath.Join(base, "root-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink pointing at a directory the predicate would accept must not
	// be returned: following it would let the sweep delete elsewhere.
	victim := filepath.Join(base, "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(base, "root-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := abandonedTestRoots(filepath.Join(base, "root-*"), func(dir string) bool {
		return dir != live
	})
	want := []string{dead}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("abandonedTestRoots() = %v, want %v", got, want)
	}
}

// TestAbandonedTestRootsBadPatternReturnsNothing: failing to enumerate is not
// evidence that anything is abandoned.
func TestAbandonedTestRootsBadPatternReturnsNothing(t *testing.T) {
	got := abandonedTestRoots("[", func(string) bool { return true })
	if got != nil {
		t.Errorf("abandonedTestRoots(bad pattern) = %v, want nil", got)
	}
}
