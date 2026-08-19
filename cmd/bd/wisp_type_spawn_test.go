package main

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/formula"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/issueops"
)

// threeIssueSubgraph is a root with two children, the shape a spawned molecule
// actually has: one root row and a majority of step rows. Classification that
// only reaches the root would pass a root-only fixture.
func threeIssueSubgraph() *TemplateSubgraph {
	root := &types.Issue{ID: "mol-x", Title: "Root", IssueType: types.TypeEpic, Priority: 2}
	stepA := &types.Issue{ID: "mol-x.a", Title: "Step A", IssueType: types.TypeTask, Priority: 2}
	stepB := &types.Issue{ID: "mol-x.b", Title: "Step B", IssueType: types.TypeTask, Priority: 2}
	return &TemplateSubgraph{
		Root:   root,
		Issues: []*types.Issue{root, stepA, stepB},
		Dependencies: []*types.Dependency{
			{IssueID: stepA.ID, DependsOnID: root.ID, Type: types.DepParentChild},
			{IssueID: stepB.ID, DependsOnID: root.ID, Type: types.DepParentChild},
		},
		IssueMap: map[string]*types.Issue{root.ID: root, stepA.ID: stepA, stepB.ID: stepB},
	}
}

// TestCloneSubgraphIntoStampsWispTypeOnEveryRow pins the property bd-2kl is
// about: a spawn's classification reaches the STEPS, not just the root. Steps
// outnumber roots by an order of magnitude in the wisps table, so a fix that
// typed only the root would leave the reported gap almost exactly as wide.
func TestCloneSubgraphIntoStampsWispTypeOnEveryRow(t *testing.T) {
	w := &recordingMolWriter{}

	if _, err := cloneSubgraphInto(context.Background(), w, threeIssueSubgraph(), CloneOptions{
		Actor:     "tester",
		Ephemeral: true,
		WispType:  types.WispTypePatrol,
	}); err != nil {
		t.Fatalf("cloneSubgraphInto: %v", err)
	}

	if len(w.created) != 3 {
		t.Fatalf("created %d issues, want the root and both steps", len(w.created))
	}
	for _, issue := range w.created {
		if issue.WispType != types.WispTypePatrol {
			t.Errorf("%q created with wisp_type %q, want %q", issue.Title, issue.WispType, types.WispTypePatrol)
		}
	}
}

// TestCloneSubgraphIntoLeavesWispTypeEmptyByDefault keeps the pre-bd-2kl
// behavior for callers that ask for nothing: an unset option must not invent a
// classification.
func TestCloneSubgraphIntoLeavesWispTypeEmptyByDefault(t *testing.T) {
	w := &recordingMolWriter{}

	if _, err := cloneSubgraphInto(context.Background(), w, threeIssueSubgraph(), CloneOptions{
		Actor:     "tester",
		Ephemeral: true,
	}); err != nil {
		t.Fatalf("cloneSubgraphInto: %v", err)
	}

	for _, issue := range w.created {
		if issue.WispType != "" {
			t.Errorf("%q created with wisp_type %q, want unclassified", issue.Title, issue.WispType)
		}
	}
}

func TestResolveWispType(t *testing.T) {
	tests := []struct {
		name      string
		flagValue string
		flagSet   bool
		vars      map[string]string
		want      types.WispType
		wantErr   string
	}{
		{
			name: "nothing declared, nothing passed",
			want: "",
		},
		{
			// The half of bd-2kl that needs no caller change: the declaration
			// patrol formulas have carried since they were written.
			name: "formula declaration is read",
			vars: map[string]string{formulaWispTypeVar: "patrol"},
			want: types.WispTypePatrol,
		},
		{
			name:      "flag beats the formula declaration",
			flagValue: "gc_report",
			flagSet:   true,
			vars:      map[string]string{formulaWispTypeVar: "patrol"},
			want:      types.WispTypeGCReport,
		},
		{
			// An explicit empty flag is how a caller spawns unclassified
			// despite a formula default — which is only expressible because
			// "set" is tracked separately from the value.
			name:      "explicit empty flag clears the formula declaration",
			flagValue: "",
			flagSet:   true,
			vars:      map[string]string{formulaWispTypeVar: "patrol"},
			want:      "",
		},
		{
			name:      "typo in the flag is refused",
			flagValue: "patroll",
			flagSet:   true,
			wantErr:   "invalid wisp-type",
		},
		{
			// Nothing read this var before bd-2kl, so a formula already
			// carrying an out-of-vocabulary value spawns fine today. Refusing
			// it would break working formulas instead of classifying anything.
			name: "out-of-vocabulary formula declaration is ignored, not fatal",
			vars: map[string]string{formulaWispTypeVar: "work-molecule"},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWispType(tt.flagValue, tt.flagSet, tt.vars)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveWispType: %v", err)
			}
			if got != tt.want {
				t.Errorf("wisp type = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildAttachCloneOptsReadsFormulaWispType covers the bond spawner, whose
// vars never pass through applyVariableDefaults on the way in — so the
// formula's declared default has to be resolved inside buildAttachCloneOpts or
// it is invisible there.
func TestBuildAttachCloneOptsReadsFormulaWispType(t *testing.T) {
	subgraph := threeIssueSubgraph()
	subgraph.VarDefs = map[string]formula.VarDef{
		formulaWispTypeVar: {Default: stringPtr("patrol")},
	}
	mol := &types.Issue{ID: "bd-patrol", Title: "Patrol", Ephemeral: true}

	opts, err := buildAttachCloneOpts(subgraph, mol, types.BondTypeSequential, nil, "", "tester", bondSpawnPhase{})
	if err != nil {
		t.Fatalf("buildAttachCloneOpts: %v", err)
	}
	if opts.WispType != types.WispTypePatrol {
		t.Errorf("WispType = %q, want %q", opts.WispType, types.WispTypePatrol)
	}

	opts, err = buildAttachCloneOpts(subgraph, mol, types.BondTypeSequential, nil, "", "tester",
		bondSpawnPhase{wispType: "escalation", wispTypeSet: true})
	if err != nil {
		t.Fatalf("buildAttachCloneOpts with --wisp-type: %v", err)
	}
	if opts.WispType != types.WispTypeEscalation {
		t.Errorf("WispType = %q, want the flag's %q", opts.WispType, types.WispTypeEscalation)
	}

	if _, err = buildAttachCloneOpts(subgraph, mol, types.BondTypeSequential, nil, "", "tester",
		bondSpawnPhase{wispType: "nonsense", wispTypeSet: true}); err == nil {
		t.Error("buildAttachCloneOpts accepted an invalid --wisp-type")
	}
}

// TestBuildUpdatePatchCarriesWispType covers the reclassification route
// (`bd update --wisp-type`), which exists so an already-written row can be
// typed without a raw UPDATE around bd's own write path.
func TestBuildUpdatePatchCarriesWispType(t *testing.T) {
	patch, err := buildUpdatePatch(map[string]interface{}{"wisp_type": "patrol"})
	if err != nil {
		t.Fatalf("buildUpdatePatch: %v", err)
	}
	if !patch.WispType.Set {
		t.Fatal("patch.WispType not set")
	}
	if patch.WispType.Value != issueops.WispType(types.WispTypePatrol) {
		t.Errorf("patch.WispType = %q, want %q", patch.WispType.Value, types.WispTypePatrol)
	}

	// The empty string is a real value here, not an absent one: it is how a
	// classification is cleared.
	patch, err = buildUpdatePatch(map[string]interface{}{"wisp_type": ""})
	if err != nil {
		t.Fatalf("buildUpdatePatch clearing: %v", err)
	}
	if !patch.WispType.Set || patch.WispType.Value != "" {
		t.Errorf("clearing patch = %+v, want Set with an empty value", patch.WispType)
	}
}
