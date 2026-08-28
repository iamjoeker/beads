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

// TestErrNotesReplacementRefused covers manifestations 1 and 2: a --notes
// write silently replacing accumulated notes, and a --notes "" from a failed
// expansion wiping the field. Both reported success and exit 0 before this.
func TestErrNotesReplacementRefused(t *testing.T) {
	tests := []struct {
		name         string
		existing     string
		updates      map[string]any
		replaceNotes bool
		wantErr      bool
		wantPhrase   string
	}{
		{
			name:       "replacing existing notes is refused",
			existing:   "three sessions of findings",
			updates:    map[string]any{"notes": "FOURTH NOTE"},
			wantErr:    true,
			wantPhrase: "--append-notes",
		},
		{
			name:       "empty value over existing notes is refused as an erase",
			existing:   "three sessions of findings",
			updates:    map[string]any{"notes": ""},
			wantErr:    true,
			wantPhrase: "erase",
		},
		{
			name:       "whitespace-only value is still an erase",
			existing:   "three sessions of findings",
			updates:    map[string]any{"notes": "\n"},
			wantErr:    true,
			wantPhrase: "erase",
		},
		{
			name:         "--replace-notes is the opt-in",
			existing:     "three sessions of findings",
			updates:      map[string]any{"notes": "deliberate reset"},
			replaceNotes: true,
			wantErr:      false,
		},
		{
			name:     "nothing to destroy on an issue with no notes",
			existing: "",
			updates:  map[string]any{"notes": "first note"},
			wantErr:  false,
		},
		{
			name:     "rewriting the identical value destroys nothing",
			existing: "unchanged",
			updates:  map[string]any{"notes": "unchanged"},
			wantErr:  false,
		},
		{
			name:     "--append-notes is never destructive",
			existing: "three sessions of findings",
			updates:  map[string]any{storageissueops.OpAppendNotes: "a fourth"},
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := errNotesReplacementRefused(tt.existing, tt.updates, tt.replaceNotes)
			if tt.wantErr != (err != nil) {
				t.Fatalf("errNotesReplacementRefused(%q, %#v, %t) error = %v, want error: %t",
					tt.existing, tt.updates, tt.replaceNotes, err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			// The message has to carry the escape hatch and say that nothing
			// was written, because the whole complaint in bd-2mx is that the
			// old report arrived after the data was already gone. The issue id
			// is deliberately absent: the caller prefixes it.
			if strings.Contains(err.Error(), "tc-1") {
				t.Errorf("refusal %q spells the issue id, which its callers already print", err.Error())
			}
			for _, want := range []string{"nothing was written", "--replace-notes", tt.wantPhrase} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q is missing %q", err.Error(), want)
				}
			}
		})
	}
}

// TestErrNotesWriteNotLanded is the post-condition half: a write whose effect
// is absent from the stored row must report failure rather than print the
// success line. It is deliberately fed a post-state that DISAGREES with the
// request — the disagreement is the whole subject.
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
