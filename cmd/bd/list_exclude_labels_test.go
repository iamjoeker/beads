package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/config"
)

// excludeLabelsCmd builds a stand-in for a work-queue listing: the two flags
// resolveExcludeLabels reads, and nothing else. The real commands are checked
// for the same pair by TestIncludeHiddenFlagRegisteredOnEveryApplyingCommand
// below, so a command that applied the default without registering the opt-out
// cannot pass both tests.
func excludeLabelsCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "listish"}
	cmd.Flags().StringSlice("exclude-label", nil, "")
	registerIncludeHiddenFlag(cmd)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags(%v): %v", args, err)
	}
	return cmd
}

// initConfigWithExcludeLabels points the process config at one value of
// BD_LIST_EXCLUDE_LABELS and rebuilds viper around it. The env var is set even
// for the "unset" case (to the empty string) rather than left absent: an
// ambient value, or the repository's own .beads/config.yaml, would otherwise
// decide the result and the test would be measuring the machine.
func initConfigWithExcludeLabels(t *testing.T, envKey, value string) {
	t.Helper()
	t.Setenv("BD_LIST_EXCLUDE_LABELS", "")
	if envKey != "" {
		t.Setenv(envKey, value)
	}
	config.ResetForTesting()
	t.Cleanup(config.ResetForTesting)
	if err := config.Initialize(); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
}

// captureStderrLines runs fn with os.Stderr redirected and returns what it
// wrote. It doubles as noise suppression: resolveExcludeLabels prints its
// notice to the real stderr otherwise.
func captureStderrLines(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	fn()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	return string(out)
}

func resolveWithCapturedNotice(t *testing.T, cmd *cobra.Command, requested []string) (labels []string, stderr string) {
	t.Helper()
	stderr = captureStderrLines(t, func() {
		labels = resolveExcludeLabels(cmd, requested)
	})
	return labels, stderr
}

func TestResolveExcludeLabels(t *testing.T) {
	// Not parallel, here or below: config is process-global viper state.
	tests := []struct {
		name       string
		configured string
		configKey  string
		args       []string
		requested  []string
		want       []string
		wantNotice bool
	}{
		{
			// The unconfigured store is the population that must see NO change
			// at all: the whole feature is opt-in.
			name:      "unset key changes nothing",
			configKey: "",
			requested: []string{"urgent"},
			want:      []string{"urgent"},
		},
		{
			name:       "configured labels apply with no flag",
			configKey:  "BD_LIST_EXCLUDE_LABELS",
			configured: "gt:message",
			want:       []string{"gt:message"},
			wantNotice: true,
		},
		{
			// The union rule. Read as "replace", `--exclude-label wontfix`
			// would bring the mail back: asking for LESS would return MORE.
			name:       "caller exclusions union with configured ones",
			configKey:  "BD_LIST_EXCLUDE_LABELS",
			configured: "gt:message",
			requested:  []string{"wontfix"},
			want:       []string{"wontfix", "gt:message"},
			wantNotice: true,
		},
		{
			name:       "--include-hidden drops the configured set whole",
			configKey:  "BD_LIST_EXCLUDE_LABELS",
			configured: "gt:message,gt:merge-request",
			args:       []string{"--include-hidden"},
			requested:  []string{"wontfix"},
			want:       []string{"wontfix"},
		},
		{
			name:       "comma-separated values are split",
			configKey:  "BD_LIST_EXCLUDE_LABELS",
			configured: "gt:message, gt:merge-request",
			want:       []string{"gt:message", "gt:merge-request"},
			wantNotice: true,
		},
		{
			// A label the caller already excluded is not excluded twice, and
			// the notice has nothing to announce.
			name:       "already-requested label is neither duplicated nor announced",
			configKey:  "BD_LIST_EXCLUDE_LABELS",
			configured: "gt:message",
			requested:  []string{"gt:message"},
			want:       []string{"gt:message"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initConfigWithExcludeLabels(t, tt.configKey, tt.configured)
			cmd := excludeLabelsCmd(t, tt.args...)

			got, stderr := resolveWithCapturedNotice(t, cmd, tt.requested)

			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("resolveExcludeLabels = %v, want %v", got, tt.want)
			}
			if gotNotice := strings.Contains(stderr, "note:"); gotNotice != tt.wantNotice {
				t.Errorf("notice printed = %v, want %v (stderr: %q)", gotNotice, tt.wantNotice, stderr)
			}
		})
	}
}

// TestResolveExcludeLabelsUnderscoreKeyFromYAML pins the alias against the
// FILE form as well as the env one. The env var cannot tell the two spellings
// apart — the key replacer maps both to BD_LIST_EXCLUDE_LABELS — so an env-only
// test would pass with the alias lookup deleted.
func TestResolveExcludeLabelsUnderscoreKeyFromYAML(t *testing.T) {
	for _, spelling := range []string{"exclude-labels", "exclude_labels"} {
		t.Run(spelling, func(t *testing.T) {
			dir := t.TempDir()
			beadsDir := filepath.Join(dir, ".beads")
			if err := os.MkdirAll(beadsDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			yaml := "list:\n  " + spelling + ": \"gt:message\"\n"
			if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(yaml), 0o600); err != nil {
				t.Fatalf("write config.yaml: %v", err)
			}
			t.Setenv("BEADS_DIR", beadsDir)
			t.Setenv("BD_LIST_EXCLUDE_LABELS", "")
			config.ResetForTesting()
			t.Cleanup(config.ResetForTesting)
			if err := config.Initialize(); err != nil {
				t.Fatalf("config.Initialize: %v", err)
			}

			// Control: the value must be visible to the config layer at all,
			// so a zero below is a statement about the filter and not about a
			// config file this test failed to make bd read.
			if got := config.GetListExcludeLabels(); len(got) != 1 || got[0] != "gt:message" {
				t.Fatalf("config.GetListExcludeLabels() = %v, want [gt:message] — the file was not read", got)
			}

			cmd := excludeLabelsCmd(t)
			got, _ := resolveWithCapturedNotice(t, cmd, nil)
			if len(got) != 1 || got[0] != "gt:message" {
				t.Errorf("resolveExcludeLabels = %v, want [gt:message]", got)
			}
		})
	}
}

func TestUnionExcludeLabels(t *testing.T) {
	merged, added := unionExcludeLabels([]string{"a", "b", "a"}, []string{"b", "c"})
	if strings.Join(merged, "|") != "a|b|c" {
		t.Errorf("merged = %v, want [a b c] (caller order first, deduplicated)", merged)
	}
	if strings.Join(added, "|") != "c" {
		t.Errorf("added = %v, want [c] — only what configuration contributed", added)
	}
}

// TestExcludeLabelsNoticeLine pins the three things the notice has to carry for
// a reader to be able to check it: what was excluded, what named it, and how to
// turn it off. A short listing that says none of those is the silent filter this
// change exists to avoid.
func TestExcludeLabelsNoticeLine(t *testing.T) {
	line := excludeLabelsNoticeLine([]string{"gt:message"})
	for _, want := range []string{`"gt:message"`, config.ListExcludeLabelsKey, "--" + includeHiddenFlag} {
		if !strings.Contains(line, want) {
			t.Errorf("notice %q does not mention %q", line, want)
		}
	}
	// It describes the filter, not an outcome it never measured.
	for _, forbidden := range []string{" rows hidden", "were hidden"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("notice %q claims an outcome no count was taken for (%q)", line, forbidden)
		}
	}
}

// TestIncludeHiddenFlagRegisteredOnEveryApplyingCommand checks the pairing that
// makes the default safe: every command that hides rows offers the flag that
// shows them again. Both halves are asserted from the same list, so adding the
// default to a further command without its opt-out fails here.
//
// countCmd and staleCmd joined the list in bd-1v3. countCmd is the one worth
// naming: it did not carry --exclude-label at all, which is why the default
// could not be applied to it, and why its number and `bd list`'s row count
// disagreed on any store that set the key.
func TestIncludeHiddenFlagRegisteredOnEveryApplyingCommand(t *testing.T) {
	for _, cmd := range []*cobra.Command{listCmd, readyCmd, blockedCmd, countCmd, staleCmd} {
		t.Run(cmd.Name(), func(t *testing.T) {
			if cmd.Flags().Lookup("exclude-label") == nil {
				t.Fatalf("bd %s applies the configured exclusions but has no --exclude-label", cmd.Name())
			}
			flag := cmd.Flags().Lookup(includeHiddenFlag)
			if flag == nil {
				t.Fatalf("bd %s has no --%s", cmd.Name(), includeHiddenFlag)
			}
			if flag.DefValue != "false" {
				t.Errorf("--%s default = %q, want false", includeHiddenFlag, flag.DefValue)
			}
			if !strings.Contains(flag.Usage, config.ListExcludeLabelsKey) {
				t.Errorf("--%s usage %q does not name the config key that hides the rows", includeHiddenFlag, flag.Usage)
			}
		})
	}
}

// TestListNamespaceIsRecognizedConfigKey guards the door the key is set
// through: an unrecognized-key warning printed over a setting that DOES take
// effect teaches the operator their config line did not work.
func TestListNamespaceIsRecognizedConfigKey(t *testing.T) {
	for _, key := range []string{config.ListExcludeLabelsKey, "list.exclude_labels", "list.limit"} {
		if !isRecognizedConfigKey(key) {
			t.Errorf("bd config set %s warns that the key is unknown", key)
		}
	}
}
