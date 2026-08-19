package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// Wisps are per-store. There is one wisps table per Dolt database, and nothing
// federates them, so `bd mol wisp list` has always answered about exactly one
// store: the one the current directory resolves to. It named neither the store
// nor that restriction, and it reports a large, plausible row count from any
// store you point it at — so a wrong-store zero and a real zero looked
// identical, and a town-wide wisp audit was not expressible at all (bd-nc4).
//
// This file supplies the second half: which stores exist, and how to query one
// that is not the current one.

// wispStoreRef names a store a wisp listing can consult.
type wispStoreRef struct {
	Database string // dolt_database the store declares ("" when metadata.json has none)
	BeadsDir string // .beads directory the store is opened from
	Rig      string // routes.jsonl path this store came from ("" for the current store)
	Current  bool   // true for the store the command would have used anyway
}

// describe renders the store the way every other cross-store message in this
// package names one, so a reader comparing a wisp listing against a routing
// notice or a not-found annotation is comparing like with like.
//
// The database name comes from the ref rather than from a fresh read of
// metadata.json: the ref holds the name the query was actually routed to, and a
// listing that named a different database than it read would be one more
// instrument to have to check.
func (s wispStoreRef) describe() string {
	out := s.BeadsDir
	if s.Database != "" {
		out = fmt.Sprintf("database %q at %s", s.Database, s.BeadsDir)
	}
	switch {
	case s.Current:
		out += " (current store)"
	case s.Rig != "":
		out += " (" + describeRigPath(s.Rig) + ")"
	}
	return out
}

// describeRigPath names a routes.jsonl target. The town's own route is recorded
// as ".", which is meaningless printed verbatim.
func describeRigPath(path string) string {
	if path == "." {
		return "the town root"
	}
	return path
}

// discoverWispStores lists every store a town-wide wisp query has to visit: the
// current one first, then every distinct rig named in routes.jsonl.
//
// Routes are the only enumeration of the town's stores that exists — the same
// file prefix routing already follows — so a store absent from routes.jsonl is
// absent here too. That is a real limit of --all-stores and the caller says so
// rather than presenting the sweep as exhaustive.
//
// Deduplication is by RESOLVED directory: several prefixes routing to one rig
// are one store, and the current store is usually also reachable through a
// route. Listing it twice would double every count.
func discoverWispStores(currentBeadsDir string) (stores []wispStoreRef, routesFile string) {
	var dirs []string
	add := func(ref wispStoreRef) {
		if ref.BeadsDir == "" {
			return
		}
		for _, seen := range dirs {
			if sameResolvedDir(seen, ref.BeadsDir) {
				return
			}
		}
		dirs = append(dirs, ref.BeadsDir)
		stores = append(stores, ref)
	}

	if currentBeadsDir != "" {
		add(wispStoreRef{
			Database: readDoltDatabase(currentBeadsDir),
			BeadsDir: currentBeadsDir,
			Current:  true,
		})
	}

	src := findPrefixRoutesSource(currentBeadsDir)
	if src == nil {
		return stores, ""
	}
	for _, route := range src.Routes {
		beadsDir := beads.FollowRedirect(filepath.Join(src.TownRoot, route.Path, ".beads"))
		add(wispStoreRef{
			Database: readDoltDatabase(beadsDir),
			BeadsDir: beadsDir,
			Rig:      route.Path,
		})
	}
	return stores, src.File
}

// selectWispStores resolves the --rig selector against the discovered stores.
//
// A selector that matches nothing is an ERROR naming what was available, never
// an empty listing: "no such rig" and "that rig holds no wisps" are the two
// answers this command tree most needs to keep apart.
func selectWispStores(stores []wispStoreRef, rig string) ([]wispStoreRef, error) {
	rig = strings.TrimSpace(rig)
	if rig == "" {
		return stores, nil
	}
	var matched []wispStoreRef
	for _, s := range stores {
		if strings.EqualFold(s.Rig, rig) || strings.EqualFold(s.Database, rig) ||
			(s.Rig != "" && strings.EqualFold(filepath.Base(s.Rig), rig)) {
			matched = append(matched, s)
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("no store named %q; known stores: %s", rig, strings.Join(wispStoreNames(stores), ", "))
	}
	return matched, nil
}

// wispStoreNames lists the selectors --rig accepts, for the error above.
func wispStoreNames(stores []wispStoreRef) []string {
	out := make([]string, 0, len(stores))
	for _, s := range stores {
		switch {
		case s.Rig != "" && s.Database != "":
			out = append(out, fmt.Sprintf("%s (database %q)", describeRigPath(s.Rig), s.Database))
		case s.Database != "":
			out = append(out, fmt.Sprintf("database %q", s.Database))
		default:
			out = append(out, s.BeadsDir)
		}
	}
	return out
}

// queryWispStore reads one store's wisps, matching the type filter and
// INCLUDING closed ones; the status scope is applied afterwards by the caller
// so the listing can report how many rows the scope hid.
//
// currentStore is the already-open handle for the store the command resolved
// to; reusing it avoids a second open of the database this process is holding.
// Any other store is opened read-only and closed here.
func queryWispStore(ctx context.Context, ref wispStoreRef, currentStore storage.DoltStorage, typeFilter string) ([]*types.Issue, error) {
	if ref.Current && currentStore != nil {
		return currentStore.SearchIssues(ctx, "", wispListFilter(typeFilter))
	}
	s, _, err := openStoreAtBeadsDir(ctx, ref.BeadsDir, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	return s.SearchIssues(ctx, "", wispListFilter(typeFilter))
}
