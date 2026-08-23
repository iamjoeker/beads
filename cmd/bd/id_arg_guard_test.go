package main

import (
	"errors"
	"strings"
	"testing"
)

// TestErrEmptyIssueIDArg covers the quoted-failed-expansion guard: an empty or
// whitespace-only positional is never a real issue ID, so close/update refuse
// it before opening the store (bd-lrk).
func TestErrEmptyIssueIDArg(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		wantPos string
	}{
		{name: "no args", args: nil},
		{name: "real id", args: []string{"bd-lrk"}},
		{name: "several real ids", args: []string{"bd-lrk", "bd-m00pb"}},
		{name: "empty string", args: []string{""}, wantErr: true, wantPos: "argument 1"},
		{name: "whitespace only", args: []string{"   "}, wantErr: true, wantPos: "argument 1"},
		{name: "tab only", args: []string{"\t"}, wantErr: true, wantPos: "argument 1"},
		{name: "newline only", args: []string{"\n"}, wantErr: true, wantPos: "argument 1"},
		{
			name:    "empty among real ids",
			args:    []string{"bd-lrk", "", "bd-m00pb"},
			wantErr: true,
			wantPos: "argument 2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := errEmptyIssueIDArg(tc.args)
			if tc.wantErr != (err != nil) {
				t.Fatalf("errEmptyIssueIDArg(%q) error = %v, wantErr %v", tc.args, err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), tc.wantPos) {
				t.Errorf("error should name %s, got: %v", tc.wantPos, err)
			}
			// The message has to point at the shell, not at a missing bead:
			// "no issue found" sends the caller looking for a deleted issue.
			if !strings.Contains(err.Error(), "expansion") {
				t.Errorf("error should name the failed expansion, got: %v", err)
			}
		})
	}
}

// TestCloseConfirmAnswer covers the answer parsing for the no-ID close
// prompt. Default-no: only an explicit yes closes an issue the command line
// never named (bd-lrk).
func TestCloseConfirmAnswer(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		readErr   error
		wantClose bool
	}{
		{name: "y", line: "y\n", wantClose: true},
		{name: "yes", line: "yes\n", wantClose: true},
		{name: "uppercase Y", line: "Y\n", wantClose: true},
		{name: "padded yes", line: "  YES  \n", wantClose: true},
		{name: "n", line: "n\n"},
		{name: "bare enter", line: "\n"},
		{name: "empty", line: ""},
		{name: "unrelated word", line: "sure\n"},
		{name: "eof with no answer", line: "", readErr: errors.New("EOF")},
		// A read error after a complete answer still honors the answer:
		// ReadString returns io.EOF alongside a final unterminated line.
		{name: "eof after yes", line: "y", readErr: errors.New("EOF"), wantClose: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := closeConfirmAnswer("bd-lrk", tc.line, tc.readErr)
			if tc.wantClose {
				if err != nil {
					t.Fatalf("closeConfirmAnswer(%q) = %v, want nil (close)", tc.line, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("closeConfirmAnswer(%q) = nil, want a refusal", tc.line)
			}
			if !strings.Contains(err.Error(), "bd-lrk") && !strings.Contains(err.Error(), "explicit issue ID") {
				t.Errorf("refusal should say what to do instead, got: %v", err)
			}
		})
	}
}
