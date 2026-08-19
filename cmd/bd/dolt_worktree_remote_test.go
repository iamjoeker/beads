//go:build cgo

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/testutil"
	"gopkg.in/yaml.v3"
)

func TestDoltRemoteAddPersistsSyncRemoteToSharedWorktreeConfig(t *testing.T) {
	skipIfNoDolt(t)
	if runtime.GOOS == "windows" {
		t.Skip("Skipping worktree test on Windows")
	}

	bd := buildBDForInitTests(t)
	bareDir, worktreeDir := setupBareParentInitWorktree(t)
	bareBeadsDir := filepath.Join(bareDir, ".beads")
	port, err := testutil.FindFreePort()
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	sharedEnv := append(os.Environ(),
		"BEADS_DOLT_SHARED_SERVER=1",
		"BEADS_DOLT_SERVER_PORT="+strconv.Itoa(port),
	)

	initCmd := exec.Command(bd, "init", "--prefix", "remote-sync", "--skip-hooks", "--quiet")
	initCmd.Dir = worktreeDir
	initCmd.Env = sharedEnv
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init from bare-parent worktree failed: %v\n%s", err, out)
	}

	remoteURL := "git+ssh://git@example.com/acme/beads.git"
	addCmd := exec.Command(bd, "dolt", "remote", "add", "origin", remoteURL)
	addCmd.Dir = worktreeDir
	addCmd.Env = sharedEnv
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("bd dolt remote add from bare-parent worktree failed: %v\n%s", err, out)
	}

	configPath := filepath.Join(bareBeadsDir, "config.yaml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("failed to read shared config.yaml: %v", err)
	}
	// Read the value, don't match the text. updateYamlKey writes a dotted key
	// flat when the document has no other mapping entries and nested once it
	// does, so the shape here depends on whether `bd init` happened to write
	// dolt.shared-server first. Both shapes read back identically; only the
	// literal-string assertion could tell them apart (bd-2k4).
	if got := yamlStringValue(t, content, "sync", "remote"); got != remoteURL {
		t.Fatalf("shared config.yaml sync.remote = %q, want %q, in:\n%s", got, remoteURL, string(content))
	}

	if _, err := os.Stat(filepath.Join(worktreeDir, ".beads")); !os.IsNotExist(err) {
		t.Fatalf("expected no worktree-local .beads directory after remote add, got err=%v", err)
	}
}

// yamlStringValue resolves a dotted config key in a config.yaml document,
// accepting either the flat form (`a.b: v`) or the nested one
// (`a:\n  b: v`). Returns "" when the key is absent in both shapes.
func yamlStringValue(t *testing.T, content []byte, parts ...string) string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		t.Fatalf("parse config.yaml: %v\n%s", err, string(content))
	}
	if flat, ok := doc[strings.Join(parts, ".")].(string); ok {
		return flat
	}
	current := doc
	for i, part := range parts {
		value, ok := current[part]
		if !ok {
			return ""
		}
		if i == len(parts)-1 {
			s, _ := value.(string)
			return s
		}
		nested, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		current = nested
	}
	return ""
}
