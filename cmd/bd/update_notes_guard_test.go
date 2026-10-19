package main

import (
	"strings"
	"testing"

	storageissueops "github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// Every case below asserts the FAILURE side: that a write which would not
// achieve what the caller asked is REFUSED or REPORTED, not that a good write
// succeeds. bd-2mx is a class of bugs whose happy path was always green — a
// test that only exercises the successful write cannot see it.

// TestErrEmptyAppendNotes covers manifestation 3: `--append-notes ""` used to
// print "✓ Updated issue" and exit 0 having written nothing.
func TestErrEmptyAppendNotes(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantErr bool
	}{
		{name: "empty is a failed expansion", text: "", wantErr: true},
		{name: "whitespace only carries nothing", text: "   ", wantErr: true},
		{name: "bare newline from a substitution", text: "\n", wantErr: true},
		{name: "real text is allowed", text: "found the leak", wantErr: false},
		{name: "text with surrounding space is allowed", text: "  note  ", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := errEmptyAppendNotes(tt.text)
			if tt.wantErr != (err != nil) {
				t.Fatalf("errEmptyAppendNotes(%q) error = %v, want error: %t", tt.text, err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "shell expansion") {
				t.Errorf("refusal should name the likely cause, got %q", err.Error())
			}
		})
	}
}

func TestErrNotesWriteNotLanded(t *testing.T) {
	tests := []struct {
		name       string
		intent     notesIntent
		after      *types.Issue
		wantErr    bool
		wantPhrase string
	}{
		{
			name:       "replace silently did not apply",
			intent:     notesIntent{replace: "new value", replaceSet: true},
			after:      &types.Issue{Notes: "old value"},
			wantErr:    true,
			wantPhrase: "--notes did not land",
		},
		{
			name:       "append silently did not apply",
			intent:     notesIntent{appended: "a fourth"},
			after:      &types.Issue{Notes: "three sessions of findings"},
			wantErr:    true,
			wantPhrase: "--append-notes did not land",
		},
		{
			name:    "replace landed",
			intent:  notesIntent{replace: "new value", replaceSet: true},
			after:   &types.Issue{Notes: "new value"},
			wantErr: false,
		},
		{
			name:    "a deliberate clear landed",
			intent:  notesIntent{replace: "", replaceSet: true},
			after:   &types.Issue{Notes: ""},
			wantErr: false,
		},
		{
			name:    "append landed at the end of the existing notes",
			intent:  notesIntent{appended: "a fourth"},
			after:   &types.Issue{Notes: "three sessions of findings\n\na fourth"},
			wantErr: false,
		},
		{
			name:    "no notes edit requested",
			intent:  notesIntent{},
			after:   &types.Issue{Notes: "untouched"},
			wantErr: false,
		},
		{
			name:    "an unavailable post-state is not a confirmation, and not a verdict either",
			intent:  notesIntent{replace: "new value", replaceSet: true},
			after:   nil,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := errNotesWriteNotLanded(tt.intent, tt.after)
			if tt.wantErr != (err != nil) {
				t.Fatalf("errNotesWriteNotLanded(%#v, %#v) error = %v, want error: %t",
					tt.intent, tt.after, err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.wantPhrase) {
				t.Errorf("failure %q is missing %q", err.Error(), tt.wantPhrase)
			}
		})
	}
}

// TestNotesIntentFromUpdates pins the mapping both routes rely on to know what
// the caller asked for. `--notes ""` must survive as a SET clear rather than
// collapsing into "no notes edit requested" — otherwise the post-condition
// check would silently stop covering the erase case.
func TestNotesIntentFromUpdates(t *testing.T) {
	tests := []struct {
		name    string
		updates map[string]any
		want    notesIntent
	}{
		{
			name:    "no notes edit",
			updates: map[string]any{"status": "open"},
			want:    notesIntent{},
		},
		{
			name:    "replace",
			updates: map[string]any{"notes": "value"},
			want:    notesIntent{replace: "value", replaceSet: true},
		},
		{
			name:    "deliberate clear stays distinguishable from absent",
			updates: map[string]any{"notes": ""},
			want:    notesIntent{replace: "", replaceSet: true},
		},
		{
			name:    "append",
			updates: map[string]any{storageissueops.OpAppendNotes: "more"},
			want:    notesIntent{appended: "more"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notesIntentFromUpdates(tt.updates); got != tt.want {
				t.Errorf("notesIntentFromUpdates(%#v) = %#v, want %#v", tt.updates, got, tt.want)
			}
		})
	}
}

// TestValidateNotesUpdateRejectsInlineEmpty pins the deliberate contrast with
// the description guard: an inline empty value IS refused here, because a
// dead command substitution reaches the flag layer as exactly that (GH#6021)
// and notes has no stdin/file input to scope the guard to. The refusal must
// name the verb that clears deliberately.
func TestValidateNotesUpdateRejectsInlineEmpty(t *testing.T) {
	err := validateNotesUpdate("")
	if err == nil {
		t.Fatal("expected inline empty notes to be rejected")
	}
	if !strings.Contains(err.Error(), "--clear-notes") {
		t.Fatalf("expected refusal to name --clear-notes, got: %v", err)
	}
}

func TestValidateNotesUpdateAllowsNonEmpty(t *testing.T) {
	if err := validateNotesUpdate("handoff context"); err != nil {
		t.Fatalf("expected non-empty notes to succeed, got: %v", err)
	}
}

// TestUpdateRegistersClearNotes guards the wiring: the update command must
// carry the verb the guard's refusal advertises. create does not register it
// — a new issue has no notes to wipe, so `bd create --notes ""` is not the
// GH#6021 hazard.
func TestUpdateRegistersClearNotes(t *testing.T) {
	if updateCmd.Flags().Lookup("clear-notes") == nil {
		t.Fatal("expected update to register --clear-notes")
	}
	if createCmd.Flags().Lookup("clear-notes") != nil {
		t.Fatal("create registers --clear-notes but has no wipe hazard; move the guard if this is now intended")
	}
}
