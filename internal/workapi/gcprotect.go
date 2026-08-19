package workapi

import (
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// The shared, DATABASE-FREE definition of which beads a BULK deletion may
// never take: `bd gc`, `bd purge`, `bd prune` and `bd mol wisp gc` all decide
// through the one set built here, so a record protected on one path is not
// deletable through the next one.
//
// WHY LABELS AND NOT STATUS. Every bulk-delete path in this repository selects
// on status ("closed rows of a tier"), and "closed means done" is FALSE for
// some of what callers put in those tiers. A merge-request record that was
// CLOSED WITHOUT MERGING is the only evidence that work did not land, and a
// delivered message is closed by being successfully processed — so for exactly
// those two classes, closure is the trigger for deletion rather than a signal
// that deletion is safe. Both live in the ephemeral tier, whose tables the
// version-control plane ignores, so a deletion there is permanent: no history,
// no backup, no undo (bd-czf).
//
// A STATUS FLAG IS NOT A CONTROL, which is why this is not "keep merge
// requests in_progress". The incident that produced this file had a P0
// merge-request record driven back into the delete set THREE times: each
// manual re-protection was undone by the next close, and nothing recorded who
// closed it. A label survives a close; a status does not.
//
// WHY IT IS CONFIGURABLE. Beads does not own the orchestration layer's
// vocabulary, so the label names are a workspace setting
// (ConfigKeyGCProtectedLabels) rather than a fact of the tracker. The built-in
// defaults exist because a protection that ships inert protects nothing: an
// unconfigured workspace running the same sweeps has the same records to lose.

// ConfigKeyGCProtectedLabels is the workspace setting naming the labels that
// hold a bead back from every bulk deletion. Its value is a comma-separated
// list; when set it REPLACES the built-in defaults, and an empty value is
// treated as unset (the same layering types.infra uses). To protect nothing,
// name a label nothing carries.
const ConfigKeyGCProtectedLabels = "gc.protected_labels"

// defaultGCProtectedLabels are the two classes an unconfigured workspace still
// must not lose in a sweep. Both are records ABOUT work rather than work
// itself, both are routinely closed while still being the only copy of what
// they record, and both were destroyed in production by a scheduled sweep
// before this protection existed.
var defaultGCProtectedLabels = []string{"gt:merge-request", "gt:message"}

// DefaultGCProtectedLabels returns the built-in protected label list.
func DefaultGCProtectedLabels() []string {
	out := make([]string, len(defaultGCProtectedLabels))
	copy(out, defaultGCProtectedLabels)
	return out
}

// GCProtectedLabels is a resolved protected-label set, keyed by normalized
// label. The zero value protects NOTHING and is never what a delete path
// should be holding: every caller resolves one with ResolveGCProtectedLabels
// (or the storage-layer wrappers around it), which falls back to the defaults.
type GCProtectedLabels map[string]bool

// NewGCProtectedLabels builds a set from an exact label list, applying no
// defaults. Empty and whitespace-only entries are dropped: a label that
// normalizes to "" would match every bead carrying an empty label string.
func NewGCProtectedLabels(labels []string) GCProtectedLabels {
	set := make(GCProtectedLabels, len(labels))
	for _, label := range labels {
		if normalized := normalizeGCLabel(label); normalized != "" {
			set[normalized] = true
		}
	}
	return set
}

// ResolveGCProtectedLabels layers the three sources a workspace can name its
// protected labels through: the stored setting, then config.yaml, then the
// built-in defaults. First non-empty wins.
//
// It is pure so that every backend resolves identically and so a caller that
// cannot reach one source still gets a protecting set rather than an empty
// one: a failed config read must fall through to the defaults, never to "no
// labels are protected". Deleting on a read failure is the shape of failure
// this whole mechanism exists to prevent.
func ResolveGCProtectedLabels(stored string, fromYAML []string) GCProtectedLabels {
	if set := NewGCProtectedLabels(splitGCLabelList(stored)); len(set) > 0 {
		return set
	}
	if set := NewGCProtectedLabels(fromYAML); len(set) > 0 {
		return set
	}
	return NewGCProtectedLabels(DefaultGCProtectedLabels())
}

// Protects reports whether this bead carries a protected label. A nil issue is
// not protected — the delete paths count an unreadable row as its own skip
// bucket rather than folding it in here.
func (p GCProtectedLabels) Protects(issue *types.Issue) bool {
	if len(p) == 0 || issue == nil {
		return false
	}
	for _, label := range issue.Labels {
		if p[normalizeGCLabel(label)] {
			return true
		}
	}
	return false
}

// Labels returns the set's members, for messages that name what protected a
// bead. Order is unspecified; callers that print them sort first.
func (p GCProtectedLabels) Labels() []string {
	out := make([]string, 0, len(p))
	for label := range p {
		out = append(out, label)
	}
	return out
}

// normalizeGCLabel is how a configured label and a stored one are compared:
// surrounding whitespace trimmed and case folded. Matching loosely can only
// ever protect MORE rows, which is the direction a deletion guard should err
// in when the two spellings disagree.
func normalizeGCLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

// splitGCLabelList parses the comma-separated form the stored setting uses.
func splitGCLabelList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.Split(value, ",")
}
