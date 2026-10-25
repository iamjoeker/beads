package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/remedy"
	"github.com/steveyegge/beads/internal/routing"
	"github.com/steveyegge/beads/internal/storage"
)

var routingConfigKeys = []string{
	"routing.mode",
	"routing.contributor",
	"routing.default",
	"routing.maintainer",
	"contributor.auto_route",
	"contributor.planning_repo",
}

func resolveRoutingConfigValue(key string, dbValues map[string]string) string {
	if src := config.GetValueSource(key); src != config.SourceDefault {
		if value := strings.TrimSpace(config.GetString(key)); value != "" {
			return value
		}
	}
	return strings.TrimSpace(dbValues[key])
}

// routingSettingsStore is the store whose settings document may supply the
// routing.* and contributor.* keys, or nil when none may.
//
// A remote backend's settings are one document shared by every client of the
// server, and the routing targets are LOCAL filesystem paths, which such a
// document cannot name for every machine reading it. Contributor routing for a
// remote workspace is therefore configured locally (config.yaml / env) only:
// `bd create`'s routing decision and the lookup fallback a missing id takes
// read the same local-only answer, and neither pays a settings round trip to
// learn it (S6b).
func routingSettingsStore(store storage.DoltStorage) storage.DoltStorage {
	if store == nil || storeIsRemoteBackend(store) {
		return nil
	}
	return store
}

func getRoutingConfigValue(ctx context.Context, store storage.DoltStorage, key string) string {
	if src := config.GetValueSource(key); src != config.SourceDefault {
		if value := strings.TrimSpace(config.GetString(key)); value != "" {
			return value
		}
	}
	store = routingSettingsStore(store)
	if store == nil {
		return ""
	}
	dbValue, err := store.GetConfig(ctx, key)
	if err != nil {
		debug.Logf("DEBUG: failed to read config %q from store: %v\n", key, err)
		return ""
	}
	return strings.TrimSpace(dbValue)
}

func determineAutoRoutedRepoPath(ctx context.Context, store storage.DoltStorage) (string, routing.RoutingRule) {
	var dbValues map[string]string
	// Local-only for a remote backend: see routingSettingsStore.
	if store = routingSettingsStore(store); store != nil {
		all, allErr := store.GetAllConfig(ctx)
		if allErr != nil {
			debug.Logf("DEBUG: failed to read config from store: %v\n", allErr)
		} else {
			dbValues = make(map[string]string, len(routingConfigKeys))
			for _, key := range routingConfigKeys {
				if v, ok := all[key]; ok {
					dbValues[key] = v
				}
			}
		}
	}

	routingMode := resolveRoutingConfigValue("routing.mode", dbValues)
	contributorRepo := resolveRoutingConfigValue("routing.contributor", dbValues)

	if routingMode == "" {
		if resolveRoutingConfigValue("contributor.auto_route", dbValues) == "true" {
			routingMode = "auto"
		}
	}
	if contributorRepo == "" {
		contributorRepo = resolveRoutingConfigValue("contributor.planning_repo", dbValues)
	}

	routingConfig := &routing.RoutingConfig{
		Mode:             routingMode,
		DefaultRepo:      resolveRoutingConfigValue("routing.default", dbValues),
		MaintainerRepo:   resolveRoutingConfigValue("routing.maintainer", dbValues),
		ContributorRepo:  contributorRepo,
		ExplicitOverride: "",
	}

	userRole := detectUserRoleForRouting(routingConfig)
	return routing.DetermineTargetRepoWithRule(routingConfig, userRole, ".")
}

// routingRuleMechanism names the routing rule that swapped the store, in a form
// that reads as the subject of a sentence, together with the command that undoes
// it.
//
// Both the list/ready notice and the not-found annotation (bd-1uu) render from
// this, so the two paths can never name different mechanisms or different fixes
// for the same rule — the whole complaint in bd-1uu is that one path discloses
// the routing decision and its sibling stays silent about the same decision.
func routingRuleMechanism(rule routing.RoutingRule) (mechanism, fix string) {
	switch rule {
	case routing.RuleContributor:
		// The contributor rule fires on explicit beads.role=contributor OR on
		// origin-URL inference with beads.role unset — don't claim the config
		// key is set when it may not be.
		return "contributor routing (beads.role=contributor, or inferred from the origin URL)",
			"git config beads.role maintainer"
	case routing.RuleMaintainer:
		return "routing.maintainer", "bd config unset routing.maintainer"
	case routing.RuleDefault:
		return "routing.default", "bd config unset routing.default"
	default:
		return "an auto-routing rule",
			"bd config get routing.mode routing.contributor routing.maintainer routing.default"
	}
}

// routingNoticeText returns the stderr note and remediation command for the
// given routing rule, so the notice always attributes the swap to the rule
// that actually matched instead of hardcoding the contributor-role case.
func routingNoticeText(rule routing.RoutingRule) (reason, fix string) {
	mechanism, fix := routingRuleMechanism(rule)
	switch rule {
	case routing.RuleContributor:
		return mechanism + " routes bd list/ready to the contributor planning store, not this project", fix
	case routing.RuleMaintainer, routing.RuleDefault:
		return mechanism + " routes bd list/ready to a different planning store than this project", fix
	default:
		return mechanism + " sends bd list/ready to a different planning store than this project", fix
	}
}

// printContributorRoutingNotice tells the user that `bd list`/`bd ready` are
// reading from an auto-routed store instead of the local project database,
// so a short or empty result doesn't look like data loss. Without this, a
// routed-but-unrelated (often empty) planning store silently replaces the
// local result set with no indication anything changed: `bd stats` and
// `bd show <id>` don't route (see openRoutedReadStore callers), so they keep
// reporting the local project truth while list/ready go quiet — exactly the
// split that makes this so hard to diagnose from the CLI alone.
//
// The notice text and remediation command are branched on rule, the
// routing.RoutingRule that actually matched in determineAutoRoutedRepoPath,
// so a maintainer- or default-routed swap doesn't get misattributed to
// beads.role=contributor.
//
// Gated on !quietFlag: --quiet is documented as "Suppress non-essential
// output (errors only)" (cmd/bd/main.go), and other non-error stderr
// notices in this package (tips.go, metrics.go) respect it the same way.
func printContributorRoutingNotice(ctx context.Context, localStore storage.DoltStorage, rule routing.RoutingRule) {
	if quietFlag {
		return
	}
	countSuffix := ""
	// countIssues reports an unfiltered COUNT(*) FROM issues, so it includes
	// closed/deferred/blocked issues that bd ready's predicates would never
	// surface anyway. Report it as the project's total issue count rather than
	// claiming all of them are "hidden as a result" of routing.
	//
	// A negative count means the store could not be counted; the notice is
	// still accurate without it, so the parenthetical is silently dropped
	// rather than surfacing a secondary error.
	if n := countIssues(ctx, localStore); n >= 0 {
		countSuffix = fmt.Sprintf(" (this project has %d total issue(s))", n)
	}
	reason, fix := routingNoticeText(rule)
	fmt.Fprintf(os.Stderr, "note: %s%s.\n", reason, countSuffix)
	fmt.Fprintln(os.Stderr, "  Fix:", fix)
}

// openRoutedReadStore opens the auto-routed target store for read commands.
// Returns routed=false when reads should stay in the current store. The
// returned routing.RoutingRule identifies which rule matched, for callers
// that print a routing notice.
func openRoutedReadStore(ctx context.Context, store storage.DoltStorage) (storage.DoltStorage, bool, routing.RoutingRule, error) {
	return openAutoRoutedStore(ctx, store, false)
}

// openRoutedWriteStore is openRoutedReadStore for a mutation command acting on
// an issue that lives in the auto-routed store. It opens the target writable so
// the mutation the user explicitly asked for can commit there.
func openRoutedWriteStore(ctx context.Context, store storage.DoltStorage) (storage.DoltStorage, bool, routing.RoutingRule, error) {
	return openAutoRoutedStore(ctx, store, true)
}

// openAutoRoutedStore opens the target selected by auto-routing. Every rule,
// maintainer, contributor and default alike, opens writable when writable is
// set, for write-intent callers (resolveAndGetIssueForMutation and its exact
// twin, bd close target resolution) that commit the change where the issue
// lives. A contributor's target is their own planning repository, the one bd
// create already writes to, so a planning issue is claimed, commented on and
// closed there (GH#2345, beads-0km). Reads stay read-only.
//
// The target opens its own dolt_database, as a prefix route does
// (openRoutedTargetStore, autoRouteTargetDatabase).
func openAutoRoutedStore(ctx context.Context, store storage.DoltStorage, writable bool) (storage.DoltStorage, bool, routing.RoutingRule, error) {
	targetStore, target, err := openRoutedStoreTarget(ctx, store, writable)
	if target == nil {
		return nil, false, routing.RuleNone, err
	}
	if err != nil {
		return nil, false, target.Rule, err
	}
	return targetStore, true, target.Rule, nil
}

// routedTarget records where auto-routing sent a command, so a caller can name
// the destination and not merely hold a handle to it. A lookup that fails in the
// routed store has to be able to say which store answered (bd-1uu).
type routedTarget struct {
	Rule     routing.RoutingRule // the rule that selected this target
	RepoPath string              // expanded path of the routed repository
	BeadsDir string              // .beads directory inside RepoPath
}

// openRoutedStoreTarget opens the auto-routed store and reports where it went.
//
// It returns a nil *routedTarget when no routing rule applies, which is the
// signal that there is no second store at all — distinct from a target that
// exists but could not be opened, where the target is returned alongside the
// error so the failure can still name it.
func openRoutedStoreTarget(ctx context.Context, store storage.DoltStorage, writable bool) (storage.DoltStorage, *routedTarget, error) {
	repoPath, rule := determineAutoRoutedRepoPath(ctx, store)
	if repoPath == "" || repoPath == "." {
		return nil, nil, nil
	}

	targetRepoPath := routing.ExpandPath(repoPath)
	target := &routedTarget{
		Rule:     rule,
		RepoPath: targetRepoPath,
		BeadsDir: filepath.Join(targetRepoPath, ".beads"),
	}
	// Refuse what admission refuses from the target's own contents first, as
	// the open below would: a write's dolt_database lookup would otherwise
	// report a historical SQLite or legacy Dolt target as a missing
	// dolt_database field.
	if _, err := admitForeignWorkspace(target.BeadsDir); err != nil {
		return nil, target, fmt.Errorf("failed to open routed store at %s: %w", remedy.PrintedValue(targetRepoPath), err)
	}
	database, err := autoRouteTargetDatabase(target.BeadsDir, writable)
	if err != nil {
		return nil, target, fmt.Errorf("failed to open routed store at %s: target has no dolt_database configured: %w", remedy.PrintedValue(targetRepoPath), err)
	}
	targetStore, err := openRoutedTargetStore(ctx, target.BeadsDir, database, writable)
	if err != nil {
		return nil, target, fmt.Errorf("failed to open routed store at %s: %w", remedy.PrintedValue(target.RepoPath), err)
	}
	return wireExternalDependencyPolicy(targetStore), target, nil
}
