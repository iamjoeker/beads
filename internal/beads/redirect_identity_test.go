package beads

import (
	"os"
	"path/filepath"
	"testing"
)

// writeIdentity writes a minimal metadata.json carrying just the two raw fields
// redirect resolution reads. It deliberately does not go through
// configfile.Config: the point of these tests is what is literally on disk.
func writeIdentity(t *testing.T, beadsDir, doltDatabase, doltMode string) {
	t.Helper()
	if err := os.MkdirAll(beadsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	body := `{"database":"beads.db","backend":"dolt","dolt_database":"` + doltDatabase + `","dolt_mode":"` + doltMode + `"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRedirect(t *testing.T, beadsDir, target string) {
	t.Helper()
	if err := os.MkdirAll(beadsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, RedirectFileName), []byte(target+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestResolveRedirectCapturesModes covers the third defect in bd-cqv: the
// source identity carried across a redirect must be distinguishable from the
// stray embedded identity that a mis-targeted create leaves behind.
func TestResolveRedirectCapturesModes(t *testing.T) {
	t.Parallel()

	t.Run("captures both declared modes", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		target := filepath.Join(root, "town", ".beads")
		writeIdentity(t, target, "hq", "server")
		source := filepath.Join(root, "rig", ".beads")
		writeIdentity(t, source, "lola", "server")
		writeRedirect(t, source, target)

		info := ResolveRedirect(source)
		if !info.WasRedirected {
			t.Fatal("expected the redirect to be followed")
		}
		if info.SourceMode != "server" {
			t.Errorf("SourceMode = %q, want %q", info.SourceMode, "server")
		}
		if info.TargetMode != "server" {
			t.Errorf("TargetMode = %q, want %q", info.TargetMode, "server")
		}
	})

	t.Run("no redirect leaves TargetMode empty", func(t *testing.T) {
		t.Parallel()
		beadsDir := filepath.Join(t.TempDir(), ".beads")
		writeIdentity(t, beadsDir, "solo", "embedded")

		info := ResolveRedirect(beadsDir)
		if info.WasRedirected {
			t.Fatal("expected no redirect")
		}
		if info.SourceMode != "embedded" {
			t.Errorf("SourceMode = %q, want %q", info.SourceMode, "embedded")
		}
		if info.TargetMode != "" {
			t.Errorf("TargetMode = %q, want empty when nothing was followed", info.TargetMode)
		}
	})
}

func TestSourceIdentityContradictsTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info SourceDatabaseInfo
		want bool
	}{
		{
			// The reported shape: a create from another rig dropped an
			// embedded metadata.json beside a rig's redirect stub, and every
			// caller in that tree then read the named database instead of the
			// rig's own.
			name: "embedded source onto server target",
			info: SourceDatabaseInfo{WasRedirected: true, SourceDatabase: "hq", SourceMode: "embedded", TargetMode: "server"},
			want: true,
		},
		{
			name: "embedded source onto proxied-server target",
			info: SourceDatabaseInfo{WasRedirected: true, SourceDatabase: "hq", SourceMode: "embedded", TargetMode: "proxied-server"},
			want: true,
		},
		{
			name: "case and whitespace do not launder the mode",
			info: SourceDatabaseInfo{WasRedirected: true, SourceDatabase: "hq", SourceMode: " Embedded ", TargetMode: "SERVER"},
			want: true,
		},
		{
			// The documented topology: a server-mode rig redirecting to a
			// shared server root, each side naming its own database.
			name: "server source onto server target",
			info: SourceDatabaseInfo{WasRedirected: true, SourceDatabase: "lola", SourceMode: "server", TargetMode: "server"},
			want: false,
		},
		{
			// A target that declares no mode proves no contradiction, so the
			// historical preserve behavior stands.
			name: "target declares no mode",
			info: SourceDatabaseInfo{WasRedirected: true, SourceDatabase: "hq", SourceMode: "embedded", TargetMode: ""},
			want: false,
		},
		{
			name: "source declares no mode",
			info: SourceDatabaseInfo{WasRedirected: true, SourceDatabase: "hq", SourceMode: "", TargetMode: "server"},
			want: false,
		},
		{
			name: "nothing to carry without a redirect",
			info: SourceDatabaseInfo{WasRedirected: false, SourceDatabase: "hq", SourceMode: "embedded", TargetMode: "server"},
			want: false,
		},
		{
			name: "no source database to carry",
			info: SourceDatabaseInfo{WasRedirected: true, SourceDatabase: "", SourceMode: "embedded", TargetMode: "server"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.info.SourceIdentityContradictsTarget(); got != tt.want {
				t.Errorf("SourceIdentityContradictsTarget() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestResolveRedirectStrayEmbeddedIdentityIsRejected is the end-to-end shape of
// the bug on real files: a rig whose .beads is a redirect stub, plus the
// embedded metadata.json a mis-targeted create dropped next to it.
func TestResolveRedirectStrayEmbeddedIdentityIsRejected(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	realBeadsDir := filepath.Join(root, "mayor", "rig", ".beads")
	writeIdentity(t, realBeadsDir, "beads", "server")

	rigBeadsDir := filepath.Join(root, "rig", ".beads")
	writeRedirect(t, rigBeadsDir, realBeadsDir)
	writeIdentity(t, rigBeadsDir, "hq", "embedded")

	info := ResolveRedirect(rigBeadsDir)
	if !info.WasRedirected {
		t.Fatal("expected the redirect to be followed")
	}
	if info.SourceDatabase != "hq" {
		t.Fatalf("SourceDatabase = %q, want %q (the raw field must still be reported)", info.SourceDatabase, "hq")
	}
	if !info.SourceIdentityContradictsTarget() {
		t.Errorf("a stray embedded identity beside a redirect to a server workspace must not be carried across")
	}
}
