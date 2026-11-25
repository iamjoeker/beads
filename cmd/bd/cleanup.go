package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
	"github.com/steveyegge/beads/internal/workapi"
	"github.com/steveyegge/beads/issueops"
)

// cleanupSkips counts the closed beads `bd admin cleanup` held back, by
// reason.
type cleanupSkips struct {
	// Protected counts beads the workspace's GC protection held back: a
	// protected label, or a protected wisp kind such as an escalation.
	Protected int
	// Pinned counts beads held back by the pinned flag — the only guard this
	// command had before bd-724.
	Pinned int
}

// filterCleanupCandidates keeps the closed beads `bd admin cleanup` may
// delete.
//
// THE GC PROTECTION BELONGS HERE for the same reason it belongs in `bd purge`:
// this command deletes closed beads in bulk, and for the protected classes
// "closed" is the trigger for deletion rather than evidence that deleting is
// safe. Until bd-724 the pinned flag was the ONLY guard on this path, so
// `bd admin cleanup --ephemeral --force` destroyed exactly the records
// `bd purge` and `bd mol wisp gc` had been taught to keep — and a protection
// that only some of the sibling sweeps honor is not a protection, it is a
// reason to believe the record is safe while one command still takes it.
//
// It is a separate function from the command body so the decision is testable
// without a SQL server: `bd admin cleanup` refuses to run in embedded mode
// (requireServerMode), which is the harness the end-to-end sweep cases use.
func filterCleanupCandidates(closed []*types.Issue, protected workapi.GCProtectedLabels) ([]*types.Issue, cleanupSkips) {
	var skips cleanupSkips
	kept := make([]*types.Issue, 0, len(closed))
	for _, issue := range closed {
		switch {
		case protected.Protects(issue):
			skips.Protected++
		case issue.Pinned:
			skips.Pinned++
		default:
			kept = append(kept, issue)
		}
	}
	return kept, skips
}

// CleanupEmptyResponse is returned when there are no closed issues to delete
type CleanupEmptyResponse struct {
	DeletedCount int    `json:"deleted_count"`
	Message      string `json:"message"`
	Filter       string `json:"filter,omitempty"`
	Ephemeral    bool   `json:"ephemeral,omitempty"`
	// RetentionRequired counts the closed issues kept because their type is
	// one the workspace retains (types.retained); absent when none were.
	RetentionRequired int `json:"retention_required,omitempty"`
}

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Delete closed issues to reduce database size",
	Long: `Delete closed issues to reduce database size.

This command permanently removes closed issues from the database.

NOTE: This command only manages issue lifecycle (closed -> deleted). For general
health checks and automatic repairs, use 'bd doctor --fix' instead.

By default, deletes ALL closed issues. Use --older-than to only delete
issues closed before a certain date.

EXAMPLES:
  bd admin cleanup --force                          # Delete all closed issues
  bd admin cleanup --older-than 30 --force          # Only issues closed 30+ days ago
  bd admin cleanup --ephemeral --force              # Only closed wisps (transient molecules)
  bd admin cleanup --dry-run                        # Preview what would be deleted

SAFETY:
- Skips pinned beads, beads carrying a GC-protected label
  (` + "`gc.protected_labels`" + `) and beads of a GC-protected wisp kind
  (escalations). No flag overrides those — delete one with ` + "`bd delete <id>`" + `.
- Requires --force flag to actually delete (unless --dry-run)
- Skips pinned issues, and issues of a type the workspace's types.retained
  setting names (their ids are idempotency keys; only bd prune/purge
  --retain-ids deletes them); --json reports the latter as retention_required
- Supports --cascade, which also deletes
  parent-child descendants; other dependents survive (refused while
  types.retained names a type, since a cascade could reach a retained issue)
- Shows preview of what will be deleted
- Use --json for programmatic output

SEE ALSO:
  bd doctor --fix    Automatic health checks and repairs (recommended for routine maintenance)
  bd admin compact   Compact old closed issues to save space`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if usesProxiedServer() {
			return HandleErrorRespectJSON("admin cleanup is not supported in proxied-server mode")
		}
		evt := metrics.NewCommandEvent("admin-cleanup")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if err := requireServerMode("cleanup"); err != nil {
			return HandleError("%v", err)
		}
		force, _ := cmd.Flags().GetBool("force")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		cascade, _ := cmd.Flags().GetBool("cascade")
		olderThanDays, _ := cmd.Flags().GetInt("older-than")
		wispOnly, _ := cmd.Flags().GetBool("ephemeral")

		if store == nil {
			if err := ensureStoreActive(); err != nil {
				return HandleError("%v", err)
			}
		}

		ctx := rootCtx
		warnIgnoredYAMLRetainedTypes()

		// Build filter for closed issues. Cleanup is a scripted sweep — opt out
		// of BEADS_MAX_ROWS (designer §4.1) so a misconfigured env doesn't abort
		// cleanup mid-run and leave the database in an unswept state.
		statusClosed := types.StatusClosed
		filter := types.IssueFilter{
			Status:        &statusClosed,
			MaxRows:       0,
			MaxRowsSource: "",
		}

		if olderThanDays > 0 {
			cutoffTime := time.Now().AddDate(0, 0, -olderThanDays)
			filter.ClosedBefore = &cutoffTime
		}

		if wispOnly {
			wispTrue := true
			filter.Ephemeral = &wispTrue
		}

		// The closed issues and types.retained come from one snapshot, so the
		// hold-back and the --cascade refusal use the setting it held.
		closedIssues, retainedSetting, err := storage.SearchIssuesWithConfig(ctx, store, "", filter, issueops.SettingKeyTypesRetained)
		if err != nil {
			return HandleError("listing issues: %v", err)
		}

		protected := resolveGCProtectedLabels(ctx, store)
		selection, err := selectCleanupCandidates(closedIssues, retainedSetting, cascade, protected)
		if err != nil {
			return HandleError("%v", err)
		}

		// stderr, and unconditional, so it survives --json and so "nothing to
		// clean up" and "nothing to clean up except the record you came for"
		// are not the same output.
		if selection.Protected > 0 {
			WarnError("kept %d protected bead(s) (%s); delete one deliberately with `bd delete <id>`",
				selection.Protected, protected.Describe())
		}
		if selection.Pinned > 0 && !jsonOutput {
			fmt.Printf("Skipping %d pinned issue(s) (protected from cleanup)\n", selection.Pinned)
		}
		if selection.RetentionRequired > 0 && !jsonOutput {
			fmt.Println(retainedTypesKeptLine(selection.RetentionRequired, "closed issue(s)"))
		}

		if len(selection.IDs) == 0 {
			if jsonOutput {
				result := CleanupEmptyResponse{
					DeletedCount:      0,
					Message:           "No closed issues to delete",
					RetentionRequired: selection.RetentionRequired,
				}
				if olderThanDays > 0 {
					result.Filter = fmt.Sprintf("older than %d days", olderThanDays)
				}
				if wispOnly {
					result.Ephemeral = true
				}
				if err := outputJSON(result); err != nil {
					return err
				}
			} else {
				msg := "No closed issues to delete"
				if wispOnly && olderThanDays > 0 {
					msg = fmt.Sprintf("No closed wisps older than %d days to delete", olderThanDays)
				} else if wispOnly {
					msg = "No closed wisps to delete"
				} else if olderThanDays > 0 {
					msg = fmt.Sprintf("No closed issues older than %d days to delete", olderThanDays)
				}
				fmt.Println(msg)
			}
			return nil
		}

		issueIDs := selection.IDs

		if !force && !dryRun {
			issueType := "closed"
			if wispOnly {
				issueType = "closed wisp"
			}
			return HandleErrorWithHint(
				fmt.Sprintf("would delete %d %s issue(s)", len(issueIDs), issueType),
				"Use --force to confirm or --dry-run to preview.")
		}

		if !jsonOutput {
			issueType := "closed"
			if wispOnly {
				issueType = "closed wisp"
			}
			if olderThanDays > 0 {
				fmt.Printf("Found %d %s issue(s) older than %d days\n", len(issueIDs), issueType, olderThanDays)
			} else {
				fmt.Printf("Found %d %s issue(s)\n", len(issueIDs), issueType)
			}
			if dryRun {
				fmt.Println(ui.RenderWarn("DRY RUN - no changes will be made"))
			}
			fmt.Println()
		}

		if err := deleteBatchReporting(issueIDs, force, dryRun, cascade, jsonOutput, nil, retentionReport(selection.RetentionRequired)); err != nil {
			return HandleError("%v", err)
		}
		return nil
	},
}

func init() {
	cleanupCmd.Flags().BoolP("force", "f", false, "Actually delete (without this flag, shows error)")
	cleanupCmd.Flags().Bool("dry-run", false, "Preview what would be deleted without making changes")
	cleanupCmd.Flags().Bool("cascade", false, "Also delete parent-child descendants; other dependents survive")
	cleanupCmd.Flags().Int("older-than", 0, "Only delete issues closed more than N days ago (0 = all closed issues)")
	cleanupCmd.Flags().Bool("ephemeral", false, "Only delete closed wisps (transient molecules)")
	// Note: cleanupCmd is added to adminCmd in admin.go
}

// cleanupSelection is what `bd admin cleanup` deletes and what it held back.
type cleanupSelection struct {
	IDs               []string
	Pinned            int
	Protected         int
	RetentionRequired int
}

// selectCleanupCandidates narrows the closed issues `bd admin cleanup` found
// to the ones it may delete: pinned issues are protected, and so are issues
// of a type the workspace retains (retainedSetting is the stored value of
// types.retained). Cleanup deletes through the Deleter role, which writes no
// tombstone, so a retained issue is never its to take.
//
// --cascade is refused while anything is retained: the role expands a cascade
// to every parent-child descendant past this selection, so it could delete a
// retained issue the selection just held back.
func selectCleanupCandidates(closed []*types.Issue, retainedSetting string, cascade bool, protected workapi.GCProtectedLabels) (cleanupSelection, error) {
	var selection cleanupSelection
	if cascade && len(workapi.RetainedTypesFromSetting(retainedSetting)) > 0 {
		return selection, fmt.Errorf("--cascade is refused while %s names a type (%q): a cascade deletes dependents past the selection, "+
			"including issues of a retained type, without retiring their ids; run without --cascade",
			issueops.SettingKeyTypesRetained, retainedSetting)
	}
	// The GC protection is applied first and is not overridable here: see
	// filterCleanupCandidates.
	unpinned, skips := filterCleanupCandidates(closed, protected)
	selection.Pinned = skips.Pinned
	selection.Protected = skips.Protected
	kept, retained := holdBackRetainedTypes(unpinned, retainedSetting)
	selection.RetentionRequired = retained
	selection.IDs = make([]string, len(kept))
	for i, issue := range kept {
		selection.IDs[i] = issue.ID
	}
	return selection, nil
}
