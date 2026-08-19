package workapi

import (
	"slices"
	"sort"
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
//
// AND WHY THE WISP KIND IS NOT. A label is a claim someone attached to a
// record; a wisp KIND is part of the record itself, in beads' own vocabulary
// (types.WispType). For the classes named in gcProtectedWispTypes the kind is
// the only signal that survives every way the label can go missing, so the
// kind guard is unconditional and layered UNDER the configurable one — see
// Protects.

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

// gcProtectedWispTypes are the wisp KINDS no bulk deletion may take, whatever
// the workspace's label configuration says.
//
// AN ESCALATION IS THE RECORD YOU CANNOT AFFORD TO LOSE BY DEFINITION: an open
// one is an incident nobody has resolved yet, and wisps are unversioned, so
// deleting one is not "closed early", it is destroyed with no undo (bd-724).
// Age is not evidence that an escalation is finished; an escalation that has
// sat untouched for a day is the one MOST likely to still matter.
//
// THIS IS NOT EXPRESSIBLE AS A LABEL, which is why the class needed a second
// axis rather than another entry in defaultGCProtectedLabels. Measured on the
// town that produced bd-724: 69 wisps carried wisp_type='escalation' and ZERO
// wisps carried any label at all, so a label-based guard held back none of
// them. `gt escalate` labels the DURABLE half of an escalation gt:escalation;
// the ephemeral half — the half a wisp GC deletes — carries the kind and
// nothing else.
//
// It is deliberately short. Every other wisp kind (heartbeat, ping, patrol,
// gc_report, recovery, error) is telemetry that a sweep exists to reclaim, and
// protecting those would turn the GC off rather than make it safe.
var gcProtectedWispTypes = []types.WispType{types.WispTypeEscalation}

// GCProtectedWispTypes returns the built-in protected wisp kinds. There is no
// setting that widens or narrows it: a caller that means to delete one names
// it to `bd delete`.
func GCProtectedWispTypes() []types.WispType {
	out := make([]types.WispType, len(gcProtectedWispTypes))
	copy(out, gcProtectedWispTypes)
	return out
}

// IsGCProtectedWispType reports whether a wisp kind is protected from bulk
// deletion. The empty kind — every non-wisp bead and every wisp created before
// the classification existed — is not protected, so this only ever holds back
// a record that classified ITSELF as one of the protected kinds.
func IsGCProtectedWispType(wispType types.WispType) bool {
	if wispType == "" {
		return false
	}
	return slices.Contains(gcProtectedWispTypes, wispType)
}

// GCProtectedLabels is a resolved protected-label set, keyed by normalized
// label. The zero value protects no LABEL and is never what a delete path
// should be holding: every caller resolves one with ResolveGCProtectedLabels
// (or the storage-layer wrappers around it), which falls back to the defaults.
//
// The built-in wisp-kind guard rides along on this type rather than beside it
// (see Protects) so that the delete paths keep asking ONE object the one
// question. A second parameter threaded through four call sites is a
// protection that a fifth call site can be written without.
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

// Protects reports whether a bulk deletion must hold this bead back: it
// carries one of this set's labels, OR it is one of the built-in protected
// wisp kinds. A nil issue is not protected — the delete paths count an
// unreadable row as its own skip bucket rather than folding it in here.
//
// THE KIND CHECK RUNS EVEN ON AN EMPTY SET, and that is the point of putting
// it here. The label half can be switched off from outside the program — leave
// gc.protected_labels unset in a workspace whose records carry no labels, or
// set it to a list that names none of them — and bd-724 is what that looks
// like in production: every escalation in the town was deletable because the
// only guard was one nothing satisfied. A guard for records with no undo must
// not have an off switch reachable by configuration.
func (p GCProtectedLabels) Protects(issue *types.Issue) bool {
	if issue == nil {
		return false
	}
	if IsGCProtectedWispType(issue.WispType) {
		return true
	}
	if len(p) == 0 {
		return false
	}
	for _, label := range issue.Labels {
		if p[normalizeGCLabel(label)] {
			return true
		}
	}
	return false
}

// Labels returns the set's configured members, for messages that name what
// protected a bead. Order is unspecified; callers that print them sort first.
// It does NOT include the wisp kinds — Describe is what prints the whole
// protecting set.
func (p GCProtectedLabels) Labels() []string {
	out := make([]string, 0, len(p))
	for label := range p {
		out = append(out, label)
	}
	return out
}

// Describe renders the whole protecting set for a skip message: the configured
// labels and the built-in wisp kinds, in that order and each sorted.
//
// It names the SET rather than the member that fired, which is what the
// label-only messages always did. Naming both axes is not cosmetic: a message
// that lists only labels invites the reading that setting gc.protected_labels
// is what kept the record, and an operator who then narrows that setting to
// clear space would expect the escalations to go and be told nothing when they
// do not.
func (p GCProtectedLabels) Describe() string {
	labels := p.Labels()
	sort.Strings(labels)

	kinds := make([]string, 0, len(gcProtectedWispTypes))
	for _, wispType := range GCProtectedWispTypes() {
		kinds = append(kinds, string(wispType))
	}
	sort.Strings(kinds)

	var parts []string
	if len(labels) > 0 {
		parts = append(parts, "labels: "+strings.Join(labels, ", "))
	}
	if len(kinds) > 0 {
		parts = append(parts, "wisp types: "+strings.Join(kinds, ", "))
	}
	return strings.Join(parts, "; ")
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
