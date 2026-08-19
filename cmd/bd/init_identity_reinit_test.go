//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// readMetadataProjectID returns the project_id recorded in a workspace's
// metadata.json.
func readMetadataProjectID(t *testing.T, beadsDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var cfg struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse metadata.json: %v\n%s", err, data)
	}
	return cfg.ProjectID
}

// bd-92m. Repairing a corrupt metadata.json with `bd init --reinit-local` must
// leave the workspace's identity where the surviving database put it.
//
// The database is the survivor of this repair — the issues, and the _project_id
// they were minted under, are all still in it — so the rebuilt metadata.json has
// exactly one correct value to write: the one already in the database. Minting a
// fresh id instead produced a workspace that read fine (bd list, bd ready) and
// failed every write command on the identity check, blaming causes ("metadata
// copied from another project", "the server endpoint changed") that a repair in
// place had not committed.
//
// TestCorruptMetadataDiagnosticsRunAndDataFailsLoud pins that reads work again
// after this same repair; reads are exactly what the identity check does not
// guard, which is why it passed throughout the regression. This is the other
// half: the repaired workspace must still be the same project.
func TestReinitLocalRepairAdoptsSurvivingDatabaseIdentity(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "rid")

	run := func(args ...string) (string, error) {
		cmd := exec.Command(bd, args...)
		cmd.Dir = dir
		cmd.Env = bdEnv(dir)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run("create", "survivor", "-p", "1"); err != nil {
		t.Fatalf("bd create: %v\n%s", err, out)
	}
	original := readMetadataProjectID(t, beadsDir)
	if original == "" {
		t.Fatal("bd init recorded no project_id; nothing for the repair to preserve")
	}

	// The partial-write window: metadata.json exists but does not parse.
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"dolt","data`), 0o600); err != nil {
		t.Fatalf("corrupt metadata.json: %v", err)
	}

	runBDInit(t, bd, dir, "--reinit-local", "--prefix", "rid")

	if repaired := readMetadataProjectID(t, beadsDir); repaired != original {
		t.Fatalf("repair minted a new project identity: %q -> %q\n"+
			"the database still holds %q, so every write command now fails the identity check",
			original, repaired, original)
	}

	// The check itself, through the binary: a write command must not refuse.
	if out, err := run("stats"); err != nil {
		t.Fatalf("bd stats after the repair: %v\n%s", err, out)
	}
}

// bd-92m, second half. A recovery line naming commands that do nothing here is
// worse than no recovery line: 'bd doctor --fix' and 'bd bootstrap' both exit 0
// in embedded mode without touching the workspace, so following the advice reads
// as a successful repair and the operator is left with the override as the only
// thing that changes anything.
func TestIdentityMismatchRecoveryIsActionableInEmbeddedMode(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, beadsDir, _ := bdInit(t, bd, "--prefix", "mis")

	// Point metadata.json at a project the database is not, the way a copied
	// or restored metadata.json does.
	const foreign = "deadbeef-0000-0000-0000-000000000000"
	dbID := readMetadataProjectID(t, beadsDir)
	if dbID == "" {
		t.Fatal("bd init recorded no project_id")
	}
	data, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	cfg["project_id"] = foreign
	patched, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal metadata.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), patched, 0o600); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	cmd := exec.Command(bd, "stats")
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	raw, err := cmd.CombinedOutput()
	out := string(raw)
	if err == nil {
		t.Fatalf("bd stats with a foreign project_id: want the identity check to fire, got success:\n%s", out)
	}

	// The repair has to be reachable from the message: the database's id is
	// what metadata.json must be set to, so the message has to carry it.
	if !strings.Contains(out, dbID) {
		t.Fatalf("recovery must name the database identity %q to be actionable:\n%s", dbID, out)
	}
	if !strings.Contains(out, "metadata.json") {
		t.Fatalf("recovery must name the file to edit:\n%s", out)
	}
	// Naming doctor/bootstrap as the fix is the defect; naming them as
	// non-repairs is the fix, so key on the line that offers them.
	if strings.Contains(out, "Recovery: run 'bd doctor --fix' or 'bd bootstrap'") {
		t.Fatalf("embedded mode must not offer 'bd doctor --fix' / 'bd bootstrap' as the repair; "+
			"both exit 0 without changing anything here:\n%s", out)
	}
}
