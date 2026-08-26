package procpressure

import "testing"

func TestParseOOMScoreAdj(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     int
		wantOK   bool
	}{
		// The kernel writes a trailing newline; a parser that does not strip it
		// would report every host as unmeasured.
		{name: "kernel default with newline", contents: "0\n", want: 0, wantOK: true},
		{name: "town value", contents: "200\n", want: 200, wantOK: true},
		{name: "protected", contents: "-1000\n", want: -1000, wantOK: true},
		{name: "killed first", contents: "1000\n", want: 1000, wantOK: true},
		{name: "surrounding whitespace", contents: "  17  ", want: 17, wantOK: true},
		{name: "empty", contents: "", wantOK: false},
		{name: "not a number", contents: "n/a\n", wantOK: false},
		// Out of range cannot have come from this interface. Reporting it would
		// put a fabricated bias in front of an operator.
		{name: "above range", contents: "1001\n", wantOK: false},
		{name: "below range", contents: "-1001\n", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseOOMScoreAdj(tt.contents)
			if ok != tt.wantOK {
				t.Fatalf("parseOOMScoreAdj(%q) ok = %v, want %v", tt.contents, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("parseOOMScoreAdj(%q) = %d, want %d", tt.contents, got, tt.want)
			}
			if !ok && got != 0 {
				t.Errorf("parseOOMScoreAdj(%q) = %d on failure, want 0", tt.contents, got)
			}
		})
	}
}

func TestSacrificialOOMScore(t *testing.T) {
	// The boundary is the whole point: 0 is the kernel default and every other
	// process shares it, so only a positive bias means "picked first".
	for adj, want := range map[int]bool{-1000: false, -1: false, 0: false, 1: true, 200: true, 1000: true} {
		if got := SacrificialOOMScore(adj); got != want {
			t.Errorf("SacrificialOOMScore(%d) = %v, want %v", adj, got, want)
		}
	}
}
