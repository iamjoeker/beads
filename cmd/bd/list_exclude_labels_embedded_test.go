//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// runBDEnv runs bd with extra environment on top of the workspace's, returning
// both streams. stderr is returned rather than folded in: the exclusion notice
// is written there precisely so a --json document stays parseable, and a test
// that read only the combined output could not tell the two apart.
func runBDEnv(t *testing.T, bd, dir string, extraEnv []string, args ...string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(bd, args...)
	cmd.Dir = dir
	cmd.Env = append(bdEnv(dir), extraEnv...)
	outBuf, errBuf, err := runCommandBuffers(t, cmd)
	if err != nil {
		t.Fatalf("bd %s failed: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, outBuf.String(), errBuf.String())
	}
	return outBuf.String(), errBuf.String()
}

// idsFromJSONArray parses the issue array bd prints on stdout for --json.
func idsFromJSONArray(t *testing.T, stdout string) []string {
	t.Helper()
	start := strings.Index(stdout, "[")
	if start < 0 {
		t.Fatalf("no JSON array in output:\n%s", stdout)
	}
	var issues []*types.IssueWithCounts
	if err := json.Unmarshal([]byte(stdout[start:]), &issues); err != nil {
		t.Fatalf("parse JSON array: %v\nraw: %s", err, stdout[start:])
	}
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestEmbeddedListExcludeLabelsConfig exercises the configured exclusion
// against a real bd binary and a real store, which is the only place the whole
// chain is visible: the config file bd actually reads, the flag parsing, the
// SQL, and which stream the notice lands on.
//
// EVERY NEGATIVE HERE IS PRECEDED BY ITS POSITIVE CONTROL. The first subtest
// asserts the mail bead IS returned by an unconfigured listing, so a later
// "not present" is a statement about the filter rather than about a bead this
// test failed to create or a probe that could never have seen it.
func TestEmbeddedListExcludeLabelsConfig(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "xl")

	// The mail bead is P0 and the work bead P1, so the mail sorts FIRST on
	// every listing and would be the row `bd ready --claim` takes. A claim that
	// lands on the work bead at the end of this test therefore says the
	// exclusion reached the claim query, not merely the rendering.
	mail := bdCreate(t, bd, dir, "Inbox message", "--type", "task", "--priority", "0", "--label", "gt:message")
	work := bdCreate(t, bd, dir, "Real work", "--type", "task", "--priority", "1")

	configured := []string{"BD_LIST_EXCLUDE_LABELS=gt:message"}

	t.Run("control_unconfigured_listing_returns_the_labeled_bead", func(t *testing.T) {
		stdout, stderr := runBDEnv(t, bd, dir, nil, "list", "--json")
		ids := idsFromJSONArray(t, stdout)
		if !containsString(ids, mail.ID) {
			t.Fatalf("unconfigured bd list did not return the labeled bead %s: %v", mail.ID, ids)
		}
		if !containsString(ids, work.ID) {
			t.Fatalf("unconfigured bd list did not return the work bead %s: %v", work.ID, ids)
		}
		if strings.Contains(stderr, "note: excluding rows") {
			t.Errorf("unconfigured bd list printed the exclusion notice:\n%s", stderr)
		}
	})

	t.Run("configured_list_hides_the_labeled_bead_and_says_so", func(t *testing.T) {
		stdout, stderr := runBDEnv(t, bd, dir, configured, "list", "--json")
		ids := idsFromJSONArray(t, stdout)
		if containsString(ids, mail.ID) {
			t.Errorf("bd list returned %s despite list.exclude-labels: %v", mail.ID, ids)
		}
		if !containsString(ids, work.ID) {
			t.Errorf("bd list dropped the work bead %s as well: %v", work.ID, ids)
		}
		// The document is on stdout and the notice on stderr; mixing them
		// would break every --json consumer this default exists to serve.
		if strings.Contains(stdout, "note:") {
			t.Errorf("the notice leaked into the JSON document:\n%s", stdout)
		}
		for _, want := range []string{"gt:message", "list.exclude-labels", "--include-hidden"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr notice does not mention %q:\n%s", want, stderr)
			}
		}
	})

	t.Run("include_hidden_restores_it", func(t *testing.T) {
		stdout, stderr := runBDEnv(t, bd, dir, configured, "list", "--json", "--include-hidden")
		if ids := idsFromJSONArray(t, stdout); !containsString(ids, mail.ID) {
			t.Errorf("--include-hidden did not restore %s: %v", mail.ID, ids)
		}
		if strings.Contains(stderr, "note: excluding rows") {
			t.Errorf("--include-hidden still printed the exclusion notice:\n%s", stderr)
		}
	})

	t.Run("caller_exclude_label_does_not_replace_the_configured_set", func(t *testing.T) {
		stdout, _ := runBDEnv(t, bd, dir, configured, "list", "--json", "--exclude-label", "wontfix")
		if ids := idsFromJSONArray(t, stdout); containsString(ids, mail.ID) {
			t.Errorf("--exclude-label wontfix brought the mail back: %v", ids)
		}
	})

	t.Run("ready_hides_it_too", func(t *testing.T) {
		control, _ := runBDEnv(t, bd, dir, nil, "ready", "--json")
		if ids := idsFromJSONArray(t, control); !containsString(ids, mail.ID) {
			t.Fatalf("control: unconfigured bd ready did not return %s: %v", mail.ID, ids)
		}

		stdout, _ := runBDEnv(t, bd, dir, configured, "ready", "--json")
		ids := idsFromJSONArray(t, stdout)
		if containsString(ids, mail.ID) {
			t.Errorf("bd ready returned %s despite list.exclude-labels: %v", mail.ID, ids)
		}
		if !containsString(ids, work.ID) {
			t.Errorf("bd ready dropped the work bead %s as well: %v", work.ID, ids)
		}
	})

	// From here the workspace is mutated, so these run last and in order.

	t.Run("config_set_writes_a_key_bd_reads_back", func(t *testing.T) {
		stdout, stderr := runBDEnv(t, bd, dir, nil, "config", "set", "list.exclude-labels", "gt:message")
		combined := stdout + stderr
		for _, warning := range []string{"unrecognized", "unknown key", "Unknown key"} {
			if strings.Contains(combined, warning) {
				t.Errorf("bd config set list.exclude-labels warned %q over a key that works:\n%s", warning, combined)
			}
		}
		// No environment variable this time: the value has to come out of the
		// config file bd just wrote, which is the setup the docs describe.
		listOut, listErr := runBDEnv(t, bd, dir, nil, "list", "--json")
		if ids := idsFromJSONArray(t, listOut); containsString(ids, mail.ID) {
			t.Errorf("the configured key did not take effect from config.yaml: %v\nstderr:\n%s", ids, listErr)
		}
	})

	t.Run("claim_skips_the_hidden_bead", func(t *testing.T) {
		// list.exclude-labels is in config.yaml from the subtest above.
		stdout, _ := runBDEnv(t, bd, dir, nil, "ready", "--claim", "--json")
		ids := idsFromJSONArray(t, stdout)
		if len(ids) != 1 {
			t.Fatalf("bd ready --claim returned %d rows, want 1: %s", len(ids), stdout)
		}
		if ids[0] != work.ID {
			t.Fatalf("bd ready --claim took %s, want the work bead %s (the excluded P0 mail must not be claimable)", ids[0], work.ID)
		}
	})
}
