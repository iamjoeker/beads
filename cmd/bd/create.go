package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/metrics"
	"github.com/steveyegge/beads/internal/remotecache"
	"github.com/steveyegge/beads/internal/routing"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/timeparsing"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/ui"
	"github.com/steveyegge/beads/internal/validation"
	"github.com/steveyegge/beads/issueops"
)

// validateCreateArgs runs as cobra's Args validation, which executes before
// PersistentPreRunE opens the store or runs migrations. It reuses
// resolveTitle — the same shared validator gatherCreateInput calls for the
// proxied-server create path — so a whitespace-only title (GH#4771) is
// rejected identically for both backends, and before any invocation that is
// guaranteed to fail wastes a store open/migration.
func validateCreateArgs(cmd *cobra.Command, args []string) error {
	markdownFile, _ := cmd.Flags().GetString("file")
	graphFile, _ := cmd.Flags().GetString("graph")
	titleFlag, _ := cmd.Flags().GetString("title")

	_, err := resolveTitle(args, titleFlag, markdownFile, graphFile)
	return err
}

var createCmd = &cobra.Command{
	Use:           "create [title]",
	GroupID:       "issues",
	Aliases:       []string{"new"},
	Short:         "Create a new issue (or batch from markdown/graph JSON)",
	Args:          cobra.MatchAll(cobra.MaximumNArgs(1), validateCreateArgs),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		CheckReadonly("create")

		evt := metrics.NewCommandEvent("create")
		defer func() {
			if c := metrics.Global(); c != nil {
				c.CloseEventAndAdd(evt)
			}
		}()

		if usesProxiedServer() {
			in, err := gatherCreateInput(cmd, args)
			if err != nil {
				return err
			}
			return runCreateProxiedServer(cmd, rootCtx, in)
		}
		file, _ := cmd.Flags().GetString("file")
		graphFile, _ := cmd.Flags().GetString("graph")

		if file != "" {
			// gatherCreateInput repeats the --file argument checks and applies
			// the plan-wide flags this route used to accept and ignore
			// (--ephemeral, --no-history, --mol-type, --validate). It is the
			// same input the proxied route reads, which is what lets both build
			// one issueops.CreateBatchRequest.
			in, err := gatherCreateInput(cmd, args)
			if err != nil {
				return err
			}
			return createIssuesFromMarkdown(rootCtx, in)
		}

		if graphFile != "" {
			if len(args) > 0 {
				return HandleError("cannot specify both title and --graph flag")
			}
			graphDryRun, _ := cmd.Flags().GetBool("dry-run")
			graphOpts := graphApplyOptionsFromFlags(cmd)
			if err := graphOpts.Validate(); err != nil {
				return HandleError("invalid graph options: %v", err)
			}
			if err := rejectSingleIssueFlagsForGraph(cmd); err != nil {
				return err
			}
			return createIssuesFromGraph(graphFile, graphDryRun, graphOpts)
		}

		titleFlag, _ := cmd.Flags().GetString("title")
		title, err := resolveTitle(args, titleFlag, "", "")
		if err != nil {
			return err
		}

		// Get silent flag
		silent, _ := cmd.Flags().GetBool("silent")

		// Warn if creating a test issue in a database with existing issues.
		// A brand-new repo with zero issues is not a "production database" (#2898).
		// store is nil here for `create --repo=<remote URL>` from a directory with
		// no local .beads/: PersistentPreRunE skips local store init entirely for
		// that path, so this check is best-effort only.
		if store != nil && isTestIssue(title) && !silent && !debug.IsQuiet() {
			if stats, err := store.GetStatistics(context.Background()); err == nil && stats != nil && stats.TotalIssues >= 5 {
				fmt.Fprintf(os.Stderr, "%s Creating test issue in production database\n", ui.RenderWarn("⚠"))
				fmt.Fprintf(os.Stderr, "  Title: %q appears to be test data\n", title)
				fmt.Fprintf(os.Stderr, "  Recommendation: Use isolated test database with --db\n")
				fmt.Fprintf(os.Stderr, "    bd --db /tmp/test-beads create %q\n", title)
			}
		}

		description, descriptionChanged, err := getDescriptionFlag(cmd)
		if err != nil {
			return err
		}
		if err := validateDescriptionUpdate(cmd, description, descriptionChanged); err != nil {
			return HandleError("%v", err)
		}

		skills, _ := cmd.Flags().GetString("skills")
		if skills != "" {
			if description != "" {
				description += "\n\n"
			}
			description += "## Required Skills\n" + skills
		}

		ctxStr, _ := cmd.Flags().GetString("context")
		if ctxStr != "" {
			if description != "" {
				description += "\n\n"
			}
			description += "## Context\n" + ctxStr
		}

		if description == "" && !isTestIssue(title) {
			if config.GetBool("create.require-description") {
				return HandleError("description is required (set create.require-description: false in config.yaml to disable)")
			}
		}

		design, _, err := getDesignFlag(cmd)
		if err != nil {
			return err
		}
		acceptance, _ := cmd.Flags().GetString("acceptance")
		notes, _ := cmd.Flags().GetString("notes")
		specID, _ := cmd.Flags().GetString("spec-id")

		priorityStr, _ := cmd.Flags().GetString("priority")
		priority, err := validation.ValidatePriority(priorityStr)
		if err != nil {
			return HandleError("%v", err)
		}

		issueType, _ := cmd.Flags().GetString("type")
		assignee, _ := cmd.Flags().GetString("assignee")
		statusFlag, _ := cmd.Flags().GetString("status")
		if statusFlag != "" {
			if !types.Status(statusFlag).IsValidWithCustom(loadEmbeddedCustomStatuses()) {
				return HandleErrorRespectJSON("invalid status %q (built-in: open, in_progress, blocked, deferred, closed, pinned, hooked; or configure custom statuses via 'bd config set status.custom')", statusFlag)
			}
		}

		labels, _ := cmd.Flags().GetStringSlice("labels")
		labelAlias, _ := cmd.Flags().GetStringSlice("label")
		if len(labelAlias) > 0 {
			labels = append(labels, labelAlias...)
		}

		explicitID, _ := cmd.Flags().GetString("id")
		parentID, _ := cmd.Flags().GetString("parent")
		externalRef, _ := cmd.Flags().GetString("external-ref")
		deps, _ := cmd.Flags().GetStringSlice("deps")
		waitsFor, _ := cmd.Flags().GetString("waits-for")
		waitsForGate, _ := cmd.Flags().GetString("waits-for-gate")
		forceCreate, _ := cmd.Flags().GetBool("force")
		repoOverride, _ := cmd.Flags().GetString("repo")
		wisp, _ := cmd.Flags().GetBool("ephemeral")
		noHistory, _ := cmd.Flags().GetBool("no-history")
		if wisp && noHistory {
			return HandleError("--ephemeral and --no-history are mutually exclusive")
		}
		storageClassFlag, _ := cmd.Flags().GetString("storage-class")
		storageClass, err := resolveStorageClass(storageClassFlag, types.IssueType(issueType).Normalize())
		if err != nil {
			return HandleError("%v", err)
		}
		// --storage-class ephemeral is the spelled-out spelling of --ephemeral
		// (Protocol v0.1 C1.4: the wisp plane is today's ephemeral-class
		// implementation). It routes to the wisp path exactly like the flag;
		// the --no-history mutual exclusion above still applies.
		if storageClass == types.StorageClassEphemeral {
			if noHistory {
				return HandleError("--storage-class ephemeral and --no-history are mutually exclusive")
			}
			wisp = true
			storageClass = "" // wisp-plane rows derive ephemeral class (C1.2); no marker cell needed
		}
		molTypeStr, _ := cmd.Flags().GetString("mol-type")
		var molType types.MolType
		if molTypeStr != "" {
			molType = types.MolType(molTypeStr)
			if !molType.IsValid() {
				return HandleError("invalid mol-type %q (must be swarm, patrol, or work)", molTypeStr)
			}
		}

		wispTypeStr, _ := cmd.Flags().GetString("wisp-type")
		var wispType types.WispType
		if wispTypeStr != "" {
			wispType = types.WispType(wispTypeStr)
			if !wispType.IsValid() {
				return HandleError("invalid wisp-type %q (must be heartbeat, ping, patrol, gc_report, recovery, error, or escalation)", wispTypeStr)
			}
		}

		eventCategory, _ := cmd.Flags().GetString("event-category")
		eventActor, _ := cmd.Flags().GetString("event-actor")
		eventTarget, _ := cmd.Flags().GetString("event-target")
		eventPayload, _ := cmd.Flags().GetString("event-payload")

		if (eventCategory != "" || eventActor != "" || eventTarget != "" || eventPayload != "") && issueType != "event" {
			return HandleError("--event-category, --event-actor, --event-target, and --event-payload flags require --type=event")
		}

		var dueAt *time.Time
		dueStr, _ := cmd.Flags().GetString("due")
		if dueStr != "" {
			t, err := timeparsing.ParseRelativeTime(dueStr, time.Now())
			if err != nil {
				return HandleError("invalid --due format %q. Examples: +6h, tomorrow, next monday, 2025-01-15", dueStr)
			}
			dueAt = &t
		}

		var deferUntil *time.Time
		deferStr, _ := cmd.Flags().GetString("defer")
		if deferStr != "" {
			t, err := timeparsing.ParseRelativeTime(deferStr, time.Now())
			if err != nil {
				return HandleError("invalid --defer format %q. Examples: +1h, tomorrow, next monday, 2025-01-15", deferStr)
			}
			// Warn if defer date is in the past (user probably meant future)
			if t.Before(time.Now()) && !silent && !debug.IsQuiet() {
				fmt.Fprintf(os.Stderr, "%s Defer date %q is in the past. Issue will appear in bd ready immediately.\n",
					ui.RenderWarn("!"), t.Local().Format("2006-01-02 15:04"))
				fmt.Fprintf(os.Stderr, "  Did you mean a future date? Use --defer=+1h or --defer=tomorrow\n")
			}
			deferUntil = &t
		}

		var metadata json.RawMessage
		if cmd.Flags().Changed("metadata") {
			metadataValue, _ := cmd.Flags().GetString("metadata")
			var metadataJSON string
			if strings.HasPrefix(metadataValue, "@") {
				filePath := metadataValue[1:]
				// #nosec G304 -- user explicitly provides file path via @file.json syntax
				data, err := os.ReadFile(filePath)
				if err != nil {
					return HandleError("failed to read metadata file %s: %v", filePath, err)
				}
				metadataJSON = string(data)
			} else {
				metadataJSON = metadataValue
			}
			if !json.Valid([]byte(metadataJSON)) {
				return HandleError("invalid JSON in --metadata: must be valid JSON")
			}
			metadata = json.RawMessage(metadataJSON)
		}

		validateTemplate, _ := cmd.Flags().GetBool("validate")
		validationMode := config.GetString("validation.on-create")
		if validateTemplate || validationMode == "error" || validationMode == "warn" {
			lintIssue := &types.Issue{
				IssueType:          types.IssueType(issueType).Normalize(),
				Description:        description,
				AcceptanceCriteria: acceptance,
			}
			if err := validation.LintIssue(lintIssue); err != nil {
				if validateTemplate || validationMode == "error" {
					return HandleError("%v", err)
				}
				fmt.Fprintf(os.Stderr, "%s %v\n", ui.RenderWarn("⚠"), err)
			}
		}

		dryRun, _ := cmd.Flags().GetBool("dry-run")

		var estimatedMinutes *int
		if cmd.Flags().Changed("estimate") {
			est, _ := cmd.Flags().GetInt("estimate")
			if est < 0 {
				return HandleError("estimate must be a non-negative number of minutes")
			}
			estimatedMinutes = &est
		}

		// Use global jsonOutput set by PersistentPreRun

		// Determine target repository using routing logic
		repoPath := "." // default to current directory
		if cmd.Flags().Changed("repo") {
			// Explicit --repo flag overrides auto-routing
			repoPath = repoOverride
		} else {
			// Auto-routing based on user role
			userRole, err := routing.DetectUserRole(".")
			if err != nil {
				debug.Logf("Warning: failed to detect user role: %v\n", err)
			}

			// Build routing config with backward compatibility for legacy contributor.* keys.
			// Prefer config.yaml values, but fall back to DB config values set by bd init --contributor.
			routingMode := getRoutingConfigValue(rootCtx, store, "routing.mode")
			contributorRepo := getRoutingConfigValue(rootCtx, store, "routing.contributor")

			// NFR-001: Backward compatibility - fall back to legacy contributor.* keys
			if routingMode == "" {
				if getRoutingConfigValue(rootCtx, store, "contributor.auto_route") == "true" {
					routingMode = "auto"
				}
			}
			if contributorRepo == "" {
				contributorRepo = getRoutingConfigValue(rootCtx, store, "contributor.planning_repo")
			}

			routingConfig := &routing.RoutingConfig{
				Mode:             routingMode,
				DefaultRepo:      getRoutingConfigValue(rootCtx, store, "routing.default"),
				MaintainerRepo:   getRoutingConfigValue(rootCtx, store, "routing.maintainer"),
				ContributorRepo:  contributorRepo,
				ExplicitOverride: repoOverride,
			}

			repoPath = routing.DetermineTargetRepo(routingConfig, userRole, ".")
		}

		renderDryRun := func() error {
			previewIssue := buildCreateIssue(createIssueParams{
				ID:                 explicitID,
				Title:              title,
				Description:        description,
				Design:             design,
				AcceptanceCriteria: acceptance,
				Notes:              notes,
				SpecID:             specID,
				Priority:           priority,
				IssueType:          types.IssueType(issueType).Normalize(),
				Assignee:           assignee,
				ExternalRef:        externalRef,
				EstimatedMinutes:   estimatedMinutes,
				Ephemeral:          wisp,
				NoHistory:          noHistory,
				StorageClass:       storageClass,
				CreatedBy:          getActorWithGit(),
				Owner:              getOwner(),
				Labels:             labels,
				MolType:            molType,
				WispType:           wispType,
				InitialStatus:      statusFlag,
				DueAt:              dueAt,
				DeferUntil:         deferUntil,
				Metadata:           metadata,
				EventKind:          eventCategory,
				Actor:              eventActor,
				Target:             eventTarget,
				Payload:            eventPayload,
			})

			if jsonOutput {
				return outputJSON(previewIssue)
			}
			renderCreateDryRunPreview(previewIssue, labels, deps)
			return nil
		}

		// A preview has to refuse whatever the real create would refuse, or it
		// is not a preview of it. Every --repo check lived past this early
		// return, so `--dry-run --repo <name>` printed "Would create issue"
		// and exited 0 for targets a real create rejects outright — the same
		// success-without-a-write the --repo route was reported for, one
		// command shape over (bd-e7v). --parent already resolved the target
		// via openDryRunTargetStore; this covers the bare preview too, and
		// running it first keeps both paths reporting the identical error.
		//
		// Remote --repo URLs are excluded deliberately: validating one means
		// syncing the remote cache, and a dry-run must not mutate it (see
		// openDryRunTargetStore).
		if dryRun && repoPath != "." && !remotecache.IsRemoteURL(repoPath) {
			targetBeadsDirPath, err := resolveRepoTargetBeadsDir(repoPath)
			if err != nil {
				return HandleError("%v", err)
			}
			if _, err := checkRepoTargetInitializable(targetBeadsDirPath); err != nil {
				return HandleError("%v", err)
			}
		}

		if dryRun && parentID == "" {
			return renderDryRun()
		}

		var targetStore storage.DoltStorage
		var remoteCache *remotecache.Cache
		// The .beads workspace a --repo create is routed to, once resolved.
		// Stays empty for an unrouted create and for a remote --repo URL,
		// whose config.yaml is not a local file to read (see
		// createTargetPrefixOverlay).
		var routedBeadsDir string
		if !dryRun && repoPath != "." {
			if remotecache.IsRemoteURL(repoPath) {
				var err error
				remoteCache, err = remotecache.DefaultCache()
				if err != nil {
					return HandleError("failed to initialize remote cache: %v", err)
				}
				if _, err := remoteCache.Ensure(rootCtx, repoPath); err != nil {
					return HandleError("failed to sync remote %s: %v", repoPath, err)
				}
				targetStore, err = remoteCache.OpenStore(rootCtx, repoPath, newDoltStoreFromConfig)
				if err != nil {
					return HandleError("failed to open remote store: %v", err)
				}
			} else {
				targetBeadsDirPath, err := resolveRepoTargetBeadsDir(repoPath)
				if err != nil {
					return HandleError("%v", err)
				}
				debug.Logf("DEBUG: Routing to target repo: %s\n", targetBeadsDirPath)
				routedBeadsDir = targetBeadsDirPath

				if err := ensureBeadsDirForPath(rootCtx, targetBeadsDirPath, store); err != nil {
					return HandleError("failed to initialize target repo: %v", err)
				}

				targetStore, err = newDoltStoreFromConfig(rootCtx, targetBeadsDirPath)
				if err != nil {
					return HandleError("failed to open target store: %v", err)
				}
			}

			// Close the original store before replacing it (it won't be used anymore)
			// Note: We don't defer-close targetStore here because PersistentPostRun
			// will close whatever store is assigned to the global `store` variable.
			// This fixes the "database is closed" error during auto-flush (GH#routing-close-bug).
			if store != nil {
				_ = store.Close() // Best effort cleanup on error path
			}

			// Replace store for remainder of create operation.
			// Must use setStore to sync cmdCtx.Store — a bare `store = targetStore`
			// leaves cmdCtx.Store pointing at the closed original, which causes
			// "store is closed" in PostRun tip auto-commit (GH#tip-closed-bug).
			setStore(targetStore)
		}

		if explicitID != "" && parentID != "" {
			return HandleError("cannot specify both --id and --parent flags")
		}

		parentLookupStore := store
		if dryRun && repoPath != "." {
			var err error
			parentLookupStore, err = openDryRunTargetStore(rootCtx, repoPath)
			if err != nil {
				return HandleError("%v", err)
			}
			defer func() { _ = parentLookupStore.Close() }()
		}

		var inheritedLabels []string
		if parentID != "" {
			ctx := rootCtx
			_, err := parentLookupStore.GetIssue(ctx, parentID)
			if err != nil {
				if errors.Is(err, storage.ErrNotFound) {
					return HandleError("parent issue %s not found", parentID)
				}
				return HandleError("failed to check parent issue: %v", err)
			}

			noInheritLabels, _ := cmd.Flags().GetBool("no-inherit-labels")
			if !noInheritLabels {
				inheritedLabels, _ = parentLookupStore.GetLabels(ctx, parentID)
			}
		}

		labels = mergeCreateLabels(labels, inheritedLabels)

		if dryRun {
			return renderDryRun()
		}

		// Parse every requested dependency edge BEFORE reserving a child ID
		// or creating anything so a malformed spec aborts with no burned
		// child ID and no orphan issue behind it.
		depSpecs, err := parseDepSpecs(deps)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		waitsForSpec, err := buildWaitsFor(waitsFor, waitsForGate, cmd.Flags().Changed("waits-for-gate"))
		if err != nil {
			return HandleError("%v", err)
		}

		// The workspace overlay that describes the database this create
		// actually writes to: the local one normally, the TARGET's when --repo
		// routed the write elsewhere (createTargetPrefixOverlay).
		prefixOverlay := createTargetPrefixOverlay(repoPath, routedBeadsDir)

		createCtx := rootCtx
		if parentID != "" {
			childID, err := store.GetNextChildID(rootCtx, parentID)
			if err != nil {
				return HandleError("%v", err)
			}
			explicitID = childID
			createCtx = storage.WithReservedChildCounter(createCtx, parentID, childID)
		}

		if explicitID != "" {
			_, err := validation.ValidateIDFormat(explicitID)
			if err != nil {
				return HandleError("%v", err)
			}

			// Validate prefix matches database prefix (YAML config takes
			// precedence over DB, except under --global — see
			// loadEmbeddedIDPrefixes). `store` is already the --repo target
			// here, so the overlay handed in has to be the target's too.
			dbPrefix, allowedPrefixes := loadEmbeddedIDPrefixesWithOverlay(prefixOverlay)

			if err := validation.ValidateIDPrefixAllowed(explicitID, dbPrefix, allowedPrefixes, forceCreate); err != nil {
				return HandleError("%v", err)
			}
		}

		issue := buildCreateIssue(createIssueParams{
			ID:                 explicitID,
			Title:              title,
			Description:        description,
			Design:             design,
			AcceptanceCriteria: acceptance,
			Notes:              notes,
			SpecID:             specID,
			Priority:           priority,
			IssueType:          types.IssueType(issueType).Normalize(),
			Assignee:           assignee,
			ExternalRef:        externalRef,
			EstimatedMinutes:   estimatedMinutes,
			Ephemeral:          wisp,
			NoHistory:          noHistory,
			StorageClass:       storageClass,
			CreatedBy:          getActorWithGit(),
			Owner:              getOwner(),
			Labels:             labels,
			MolType:            molType,
			WispType:           wispType,
			EventKind:          eventCategory,
			Actor:              eventActor,
			Target:             eventTarget,
			Payload:            eventPayload,
			InitialStatus:      statusFlag,
			DueAt:              dueAt,
			DeferUntil:         deferUntil,
			Metadata:           metadata,
		})

		ctx := createCtx

		// Resolve partial --deps targets the way `bd dep add` does, so a bare
		// slug becomes a qualified id rather than a dangling edge, and an
		// unknown target fails closed here instead of reaching the write.
		resolvedDepSpecs, resolveErr := resolveDepSpecTargets(ctx, store, depSpecs)
		if resolveErr != nil {
			return HandleErrorRespectJSON("%v", resolveErr)
		}
		depSpecs = resolvedDepSpecs

		// If a discovered-from dependency is present, inherit source_repo
		// from the referenced parent issue. Reuse the already-parsed specs
		// (not the raw --deps strings) so this can't drift from parseDepSpec's
		// normalization rules.
		if dfParent := discoveredFromParentSpec(depSpecs); dfParent != "" {
			parentIssue, err := store.GetIssue(ctx, dfParent)
			if err == nil && parentIssue.SourceRepo != "" {
				issue.SourceRepo = parentIssue.SourceRepo
			}
			// If error getting parent or parent has no source_repo, continue with default
		}

		ops, err := writeOps(store)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		opsCtx, err := issueOpsContext(ctx)
		if err != nil {
			return HandleErrorRespectJSON("%v", err)
		}
		// Label inheritance stays CLI-side (mergeCreateLabels above) because the
		// dry-run preview needs it too; asking the facade to inherit as well
		// would append the parent's labels a second time.
		result, err := ops.Create(opsCtx, issueops.CreateRequest{
			Actor:         actor,
			Issue:         issue,
			ParentID:      parentID,
			Dependencies:  createDependencyRequests(depSpecs),
			WaitsFor:      waitsForRequest(waitsForSpec),
			ForceIDPrefix: forceCreate,
			IDPrefix:      createIDPrefixOverrideFrom(prefixOverlay),
		})
		if err != nil {
			// RULING R1: an occupied --id is a refusal, not a silent full-row
			// upsert reported as success.
			if errors.Is(err, storage.ErrAlreadyExists) && explicitID != "" {
				return HandleErrorRespectJSON("%s already exists; use bd update, or bd import for upsert semantics", explicitID)
			}
			return HandleErrorRespectJSON("%v", err)
		}
		// Every post-write read comes from the facade's result snapshot, never
		// from the local struct: the facade clones its request, so the local
		// struct still has no ID for an auto-minted create and no persisted
		// timestamps. Dependencies and comments are dropped because `bd create`
		// has never printed them.
		created := result.Issue
		created.Dependencies = nil
		created.Comments = nil

		edges := createDepEdges{parentID: parentID, specs: depSpecs, waitsFor: waitsForSpec}
		if edges.empty() {
			// Bare create: preserve the embedded-mode follow-up Dolt commit.
			// The deps path commits inside its transaction instead.
			shouldCommit, err := shouldCommitCreatePostWrites(created, false)
			if err != nil {
				return HandleError("dolt auto-commit failed: %v", err)
			}
			if shouldCommit {
				commitMsg := fmt.Sprintf("bd: create %s", created.ID)
				if err := store.Commit(ctx, commitMsg); err != nil && !isDoltNothingToCommit(err) {
					WarnError("failed to commit: %v", err)
				}
			}
		}

		if repoPath != "." && targetStore != nil {
			if err := commitPendingIfEmbedded(ctx, targetStore, actor, doltAutoCommitParams{
				Command:  "create",
				IssueIDs: []string{created.ID},
			}); err != nil {
				debug.Logf("warning: failed to commit routed repo: %v", err)
			}
		}

		if remoteCache != nil {
			if pushErr := remoteCache.Push(rootCtx, repoPath); pushErr != nil {
				return HandleError("failed to push to %s: %v\nThe issue was created locally but not synced to the remote.", repoPath, pushErr)
			}
		}

		if jsonOutput {
			if err := outputJSON(created); err != nil {
				return err
			}
		} else if silent {
			fmt.Println(created.ID)
		} else {
			debug.PrintNormal("%s Created issue: %s\n", ui.RenderPass("✓"), formatFeedbackID(created.ID, created.Title))
			debug.PrintNormal("  Priority: P%d\n", created.Priority)
			debug.PrintNormal("  Status: %s\n", created.Status)

			maybeShowTip(store)
		}

		SetLastTouchedID(created.ID)
		return nil
	},
}

type createIssueParams struct {
	ID                 string
	Title              string
	Description        string
	Design             string
	AcceptanceCriteria string
	Notes              string
	SpecID             string
	Priority           int
	IssueType          types.IssueType
	Assignee           string
	ExternalRef        string
	EstimatedMinutes   *int
	Ephemeral          bool
	NoHistory          bool
	StorageClass       types.StorageClass
	CreatedBy          string
	Owner              string
	Labels             []string
	MolType            types.MolType
	WispType           types.WispType
	EventKind          string
	Actor              string
	Target             string
	Payload            string
	InitialStatus      string
	DueAt              *time.Time
	DeferUntil         *time.Time
	Metadata           json.RawMessage
}

// resolveStorageClass resolves the effective storage class at create time
// (Protocol v0.1 C1.3): the explicit --storage-class flag wins; otherwise the
// per-type config default storage-class.<type> applies; otherwise unset.
// Versioned normalizes to unset — the class marker is omitted when versioned
// (C2.4), and both spell identical semantics (C1.2). Values are validated
// wherever they came from: a bad flag is a usage error, a bad config value is
// a config bug and fails just as loudly.
func resolveStorageClass(explicit string, issueType types.IssueType) (types.StorageClass, error) {
	raw := explicit
	if raw == "" {
		raw = config.GetString("storage-class." + string(issueType))
		if raw == "" {
			return "", nil
		}
	}
	class, err := types.ParseStorageClass(raw)
	if err != nil {
		if explicit == "" {
			return "", fmt.Errorf("config storage-class.%s: %w", issueType, err)
		}
		return "", err
	}
	if class == types.StorageClassVersioned {
		return "", nil
	}
	return class, nil
}

func buildCreateIssue(params createIssueParams) *types.Issue {
	var externalRefPtr *string
	if params.ExternalRef != "" {
		externalRefPtr = &params.ExternalRef
	}

	status := types.StatusOpen
	if params.InitialStatus != "" {
		status = types.Status(params.InitialStatus)
	} else if params.DeferUntil != nil && params.DeferUntil.After(time.Now()) {
		status = types.StatusDeferred
	}

	return &types.Issue{
		ID:                 params.ID,
		Title:              params.Title,
		Description:        params.Description,
		Design:             params.Design,
		AcceptanceCriteria: params.AcceptanceCriteria,
		Notes:              params.Notes,
		SpecID:             params.SpecID,
		Status:             status,
		Priority:           params.Priority,
		IssueType:          params.IssueType,
		Assignee:           params.Assignee,
		ExternalRef:        externalRefPtr,
		EstimatedMinutes:   params.EstimatedMinutes,
		Ephemeral:          params.Ephemeral,
		NoHistory:          params.NoHistory,
		StorageClass:       params.StorageClass,
		CreatedBy:          params.CreatedBy,
		Owner:              params.Owner,
		Labels:             append([]string(nil), params.Labels...),
		MolType:            params.MolType,
		WispType:           params.WispType,
		EventKind:          params.EventKind,
		Actor:              params.Actor,
		Target:             params.Target,
		Payload:            params.Payload,
		DueAt:              params.DueAt,
		DeferUntil:         params.DeferUntil,
		Metadata:           params.Metadata,
	}
}

func mergeCreateLabels(labels, inheritedLabels []string) []string {
	merged := make([]string, 0, len(labels)+len(inheritedLabels))
	seen := make(map[string]struct{}, len(labels)+len(inheritedLabels))
	for _, label := range labels {
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		merged = append(merged, label)
	}
	for _, label := range inheritedLabels {
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		merged = append(merged, label)
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// createIDPrefixOverride is the prefix an explicit --id must match, when the
// WORKSPACE knows better than the database does.
//
// config.yaml's `issue-prefix` wins over the database's, except under --global
// where the shared database is authoritative (GH#4957, selectCreateIDPrefix).
// Only a front door can read config.yaml — a shared server's database knows
// only its own prefix — so both routes resolve it here and hand it to the role
// as CreateRequest.IDPrefix. Empty means "the substrate's prefix is right",
// which is the ordinary case.
func createIDPrefixOverride() string {
	return createIDPrefixOverrideFrom(overlayYAMLPrefix(""))
}

// createIDPrefixOverrideFrom is createIDPrefixOverride for a caller that has
// already resolved which workspace's overlay applies — see
// createTargetPrefixOverlay, which is not always the workspace bd runs in.
func createIDPrefixOverrideFrom(yamlPrefix string) string {
	if globalFlag {
		return ""
	}
	return yamlPrefix
}

// createTargetPrefixOverlay resolves the config.yaml `issue-prefix` overlay
// that describes the database a create writes to.
//
// The overlay above exists because the workspace bd runs in can know its
// shared server's database better than that database knows itself (GH#4957).
// That standing covers the local workspace and nothing else. A create routed
// by --repo writes to another project's store, where the local config.yaml has
// no authority at all — reading it there splits the store from the naming
// exactly the way bd-5ut reported: an id carrying the target's own prefix is
// refused as a mismatch against a prefix belonging to the workspace the
// command was typed in, and under --force the same split writes a bead into
// the target under a prefix that store never mints.
//
// So a routed create reads the TARGET's overlay, by the same rule and for the
// same reason. beadsDir is empty for a remote --repo URL, whose config.yaml is
// not a local file to read; that falls back to "" — the target substrate's own
// prefix is authoritative, which is the ordinary answer anyway.
func createTargetPrefixOverlay(repoPath, beadsDir string) string {
	if repoPath == "." {
		return overlayYAMLPrefix("")
	}
	if beadsDir == "" {
		return ""
	}
	return strings.TrimSpace(config.GetStringFromDir(beadsDir, "issue-prefix"))
}

func selectCreateIDPrefix(global bool, yamlPrefix, storePrefix string) string {
	if global {
		return storePrefix
	}
	if yamlPrefix != "" {
		return yamlPrefix
	}
	return storePrefix
}

func renderCreateDryRunPreview(issue *types.Issue, labels, deps []string) {
	idDisplay := issue.ID
	if idDisplay == "" {
		idDisplay = "(will be generated)"
	}
	fmt.Printf("%s [DRY RUN] Would create issue:\n", ui.RenderWarn("⚠"))
	fmt.Printf("  ID: %s\n", idDisplay)
	fmt.Printf("  Title: %s\n", issue.Title)
	fmt.Printf("  Type: %s\n", issue.IssueType)
	fmt.Printf("  Priority: P%d\n", issue.Priority)
	fmt.Printf("  Status: %s\n", issue.Status)
	if issue.Assignee != "" {
		fmt.Printf("  Assignee: %s\n", issue.Assignee)
	}
	if issue.Description != "" {
		fmt.Printf("  Description: %s\n", issue.Description)
	}
	if len(labels) > 0 {
		fmt.Printf("  Labels: %s\n", strings.Join(labels, ", "))
	}
	if len(deps) > 0 {
		fmt.Printf("  Dependencies: %s\n", strings.Join(deps, ", "))
	}
	if issue.EventKind != "" {
		fmt.Printf("  Event category: %s\n", issue.EventKind)
	}
}

func shouldCommitCreatePostWrites(_ *types.Issue, _ bool) (bool, error) {
	return embeddedWritesCommitNow()
}

func createDepsAcceptedTypeList() string {
	names := []string{"blocked-by", "depends-on"}
	for _, depType := range types.WellKnownDependencyTypes() {
		names = append(names, string(depType))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func init() {
	createCmd.Flags().StringP("file", "f", "", "Create multiple issues from markdown file")
	createCmd.Flags().String("graph", "", "Create a graph of issues with dependencies from JSON plan file")
	createCmd.Flags().String("title", "", "Issue title (alternative to positional argument)")
	createCmd.Flags().Bool("silent", false, "Output only the issue ID (for scripting)")
	createCmd.Flags().Bool("dry-run", false, "Preview what would be created without actually creating")
	registerPriorityFlag(createCmd, "2")
	createCmd.Flags().StringP("type", "t", "task", "Issue type (bug|feature|task|epic|chore|decision|spike|story|milestone); custom types require types.custom config; aliases: enhancement/feat→feature, dec/adr→decision")
	createCmd.Flags().StringP("status", "s", "", "Initial status")
	registerCommonIssueFlags(createCmd)
	createCmd.Flags().String("spec-id", "", "Link to specification document")
	createCmd.Flags().StringSliceP("labels", "l", []string{}, "Labels (comma-separated)")
	createCmd.Flags().String("skills", "", "Required skills for this issue")
	createCmd.Flags().String("context", "", "Additional context for the issue")
	createCmd.Flags().StringSlice("label", []string{}, "Alias for --labels")
	_ = createCmd.Flags().MarkHidden("label") // Only fails if flag missing (caught in tests)
	createCmd.Flags().String("id", "", "Explicit issue ID (e.g., 'bd-42' for partitioning)")
	createCmd.Flags().String("parent", "", "Parent issue ID for hierarchical child (e.g., 'bd-a3f8e9')")
	createCmd.Flags().Bool("no-inherit-labels", false, "Don't inherit labels from parent issue")
	createCmd.Flags().StringSlice("deps", []string{}, "Dependencies as 'type:id' or bare 'id'. Bare 'id', 'depends-on:id', and 'blocked-by:id' all make THIS issue depend on id; 'blocks:id' reverses direction (id depends on this issue). E.g. 'blocked-by:bd-20,discovered-from:bd-15'")
	createCmd.Flags().String("waits-for", "", "Spawner issue ID to wait for (creates waits-for dependency for fanout gate)")
	createCmd.Flags().String("waits-for-gate", "all-children", "Gate type: all-children (wait for all) or any-children (wait for first)")
	createCmd.Flags().Bool("force", false, "Force creation even if prefix doesn't match database prefix")
	createCmd.Flags().String("repo", "", "Target repository for issue (overrides auto-routing)")
	createCmd.Flags().IntP("estimate", "e", 0, "Time estimate in minutes (e.g., 60 for 1 hour)")
	createCmd.Flags().Bool("ephemeral", false, "Create as ephemeral (short-lived, subject to TTL compaction)")
	createCmd.Flags().Bool("no-history", false, "Skip Dolt commit history without making GC-eligible (for permanent agent beads)")
	createCmd.Flags().String("storage-class", "", "Storage class: versioned, unversioned, or ephemeral (default: storage-class.<type> config, else versioned)")
	createCmd.Flags().String("mol-type", "", "Molecule type: swarm (multi-agent), patrol (recurring ops), work (default)")
	createCmd.Flags().String("wisp-type", "", "Wisp type for TTL-based compaction: heartbeat, ping, patrol, gc_report, recovery, error, escalation")
	createCmd.Flags().Bool("validate", false, "Validate description contains required sections for issue type")
	createCmd.Flags().Bool("allow-empty-description", false, "Allow empty description input from stdin or file")
	// Event-specific flags (only valid when --type=event)
	createCmd.Flags().String("event-category", "", "Event category (e.g., patrol.muted, agent.started) (requires --type=event)")
	createCmd.Flags().String("event-actor", "", "Entity URI who caused this event (requires --type=event)")
	createCmd.Flags().String("event-target", "", "Entity URI or bead ID affected (requires --type=event)")
	createCmd.Flags().String("event-payload", "", "Event-specific JSON data (requires --type=event)")
	// Time-based scheduling flags (GH#820)
	// Examples:
	//   --due=+6h           Due in 6 hours
	//   --due=tomorrow      Due tomorrow
	//   --due="next monday" Due next Monday
	//   --due=2025-01-15    Due on specific date
	//   --defer=+1h         Hidden from bd ready for 1 hour
	//   --defer=tomorrow    Hidden until tomorrow
	createCmd.Flags().String("due", "", "Due date/time. Formats: +6h, +1d, +2w, tomorrow, next monday, 2025-01-15")
	createCmd.Flags().String("defer", "", "Defer until date (issue hidden from bd ready until then). Same formats as --due")
	createCmd.Flags().String("metadata", "", "Set custom metadata (JSON string or @file.json to read from file)")
	// Note: --json flag is defined as a persistent flag in main.go, not here
	rootCmd.AddCommand(createCmd)
}

// formatTimeForRPC converts a *time.Time to RFC3339 string for RPC calls.
// Returns empty string if t is nil, to distinguish "not set" from "set to zero".
func formatTimeForRPC(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// openDryRunTargetStore opens the store a `create --dry-run --repo <other>`
// resolves --parent against. It is read-only on BOTH paths and must stay that
// way: newDoltStoreFromConfig runs schema initialization on whatever it opens
// and can rename a legacy hyphenated database and rewrite the target's
// metadata.json on the way (GH#3231), so using it here would have a dry-run
// mutate a repository the user only named as a lookup target — the same
// migrate-at-open trap this preview policy exists to close, one repo over.
// newPreviewStoreFromConfig is the non-mutating factory for a foreign
// project (bd-6dnrw.32), relaxed for previews exactly as the root pre-run
// relaxes the command's own store.
func openDryRunTargetStore(ctx context.Context, repoPath string) (storage.DoltStorage, error) {
	if remotecache.IsRemoteURL(repoPath) {
		cache, err := remotecache.DefaultCache()
		if err != nil {
			return nil, fmt.Errorf("failed to initialize remote cache: %w", err)
		}
		// The dry-run parent lookup only reads from this cached remote store.
		// Do not add writes here; dry-runs must not mutate cached remotes.
		store, err := cache.OpenStore(ctx, repoPath, newPreviewStoreFromConfig)
		if err != nil {
			return nil, fmt.Errorf("dry-run parent lookup requires an existing cached remote store for %s: %w", repoPath, err)
		}
		return store, nil
	}

	beadsDir, err := resolveRepoTargetBeadsDir(repoPath)
	if err != nil {
		return nil, err
	}
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	if _, err := os.Stat(metadataPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("target repo %s is not initialized; refusing to initialize it during dry-run", routing.ExpandPath(repoPath))
		}
		return nil, fmt.Errorf("failed to inspect target repo %s: %w", routing.ExpandPath(repoPath), err)
	}

	store, err := newPreviewStoreFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open target store for dry-run: %w", err)
	}
	return store, nil
}

// resolveRepoTargetBeadsDir resolves the .beads workspace that an explicit
// `--repo <path>` targets, for the local (non-remote-URL) case.
//
// Two silent-write-loss traps live here (bd-1yi), and both end the same way:
// a success line quoting an issue ID that no later read can resolve.
//
//  1. --repo takes a PATH, but a plausible repository NAME ("gastown") is easy
//     to pass instead. Nothing used to check that the path exists, so
//     ensureBeadsDirForPath's MkdirAll invented <cwd>/<name>/.beads and the
//     issue landed in a workspace nobody would ever look in. Requiring an
//     existing directory turns that into a non-zero exit before an ID is
//     minted.
//  2. A target whose .beads is a redirect stub (a rig pointing at the real
//     workspace elsewhere) has no metadata.json of its own. Without following
//     the redirect, ensureBeadsDirForPath reads the stub as uninitialized and
//     initializes an EMBEDDED database beside it, so the write lands in a
//     store the configured backend — a shared Dolt server, typically — never
//     reads.
func resolveRepoTargetBeadsDir(repoPath string) (string, error) {
	targetPath := routing.ExpandPath(repoPath)
	if targetPath == "" {
		return "", fmt.Errorf("--repo requires a repository path")
	}

	info, err := os.Stat(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("--repo target %q does not exist (--repo takes a path to a repository, not a repository name)", repoPath)
		}
		return "", fmt.Errorf("failed to inspect --repo target %s: %w", targetPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--repo target %q is not a directory", repoPath)
	}

	return beads.FollowRedirect(filepath.Join(targetPath, ".beads")), nil
}

// checkRepoTargetInitializable reports whether a --repo target's
// redirect-resolved .beads directory is already an initialized workspace, and
// refuses the two shapes ensureBeadsDirForPath must never materialize (its doc
// comment explains why each one strands the write).
//
// It only stats; the refusals therefore also hold for `--dry-run`, which must
// not create the workspace it is previewing a create into. Keeping the checks
// here rather than inline in ensureBeadsDirForPath is what lets the preview
// and the real create refuse identically, with the same wording (bd-e7v).
func checkRepoTargetInitializable(beadsDir string) (initialized bool, err error) {
	// metadata.json is the canonical marker for an initialized beads dir.
	if _, err := os.Stat(filepath.Join(beadsDir, "metadata.json")); err == nil {
		return true, nil
	}

	if _, err := os.Stat(filepath.Join(beadsDir, beads.RedirectFileName)); err == nil {
		return false, fmt.Errorf("--repo target has a redirect at %s that bd could not resolve to a database; initializing one here instead would write an identity file beside the redirect, which overrides it for every command run in that tree — repair the redirect target, or run 'bd init' in the workspace it should point at", beadsDir)
	}

	if !isEmbeddedMode() {
		return false, fmt.Errorf("--repo target has no beads workspace at %s, and this one is server-backed; the embedded database bd would create there is invisible to the shared server every other client reads from — run 'bd init' in the target repository first", beadsDir)
	}

	return false, nil
}

// ensureBeadsDirForPath ensures a beads directory exists at beadsDir.
// If it doesn't exist, it creates it and initializes with the same prefix as
// the source store (T010, T012: prefix inheritance). Callers pass the
// redirect-resolved .beads directory (see resolveRepoTargetBeadsDir), not the
// repository root.
//
// Auto-initialization is deliberately narrow (bd-cqv). Two shapes reach here
// with no metadata.json and must NOT be materialized as a fresh embedded
// database, because in both the created store is one no later read consults:
//
//  1. beadsDir still holds a redirect file. resolveRepoTargetBeadsDir already
//     followed every redirect it could, so a redirect surviving to this point
//     is one FollowRedirect refused (missing target, target with no database,
//     or a chain) and fell back from. Initializing here writes an identity
//     file next to that redirect — and an identity file beside a redirect wins
//     over it for every other caller in that tree, not just for this create.
//  2. This invocation is server-backed. A new embedded store under
//     .beads/embeddeddolt/ is invisible to the shared Dolt server that every
//     other agent in the target reads from.
func ensureBeadsDirForPath(ctx context.Context, beadsDir string, sourceStore storage.DoltStorage) error {
	initialized, err := checkRepoTargetInitializable(beadsDir)
	if err != nil {
		return err
	}
	if initialized {
		return nil
	}

	// Create .beads directory
	if err := os.MkdirAll(beadsDir, 0750); err != nil {
		return fmt.Errorf("cannot create .beads directory: %w", err)
	}

	// Initialize database via NewFromConfigWithOptions to respect Dolt config.
	// Set the prefix if source store has one (T012: prefix inheritance).
	if sourceStore != nil {
		sourcePrefix, err := sourceStore.GetConfig(ctx, "issue_prefix")
		if err == nil && sourcePrefix != "" {
			// Sanitize prefix for SQL database name (same as bd init).
			dbName := strings.ReplaceAll(sourcePrefix, "-", "_")

			// Open target store temporarily to set prefix.
			// Use newDoltStore with explicit config since the target .beads
			// directory was just created and has no metadata.json yet.
			tempStore, err := newDoltStore(ctx, &dolt.Config{
				BeadsDir:        beadsDir,
				Database:        dbName,
				CreateIfMissing: true,
			})
			if err != nil {
				return fmt.Errorf("failed to initialize target database: %w", err)
			}
			if err := tempStore.SetConfig(ctx, "issue_prefix", sourcePrefix); err != nil {
				_ = tempStore.Close() // Best effort cleanup on error path
				return fmt.Errorf("failed to set prefix in target store: %w", err)
			}
			if err := tempStore.Close(); err != nil {
				return fmt.Errorf("failed to close target store: %w", err)
			}

			// Write metadata.json so newDoltStoreFromConfig can find the
			// correct database name on subsequent opens (GH#2988).
			cfg := configfile.DefaultConfig()
			cfg.Backend = configfile.BackendDolt
			cfg.DoltDatabase = dbName
			cfg.DoltMode = configfile.DoltModeEmbedded
			cfg.ProjectID = configfile.GenerateProjectID()
			if err := cfg.Save(beadsDir); err != nil {
				return fmt.Errorf("failed to write metadata.json: %w", err)
			}
		}
	}

	return nil
}
