package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
	"github.com/steveyegge/beads/internal/utils"
)

// isNotFoundErr returns true if the error indicates the issue was not found.
// This covers both storage.ErrNotFound (from GetIssue) and the plain error
// from ResolvePartialID which doesn't wrap the sentinel.
func isNotFoundErr(err error) bool {
	if errors.Is(err, storage.ErrNotFound) {
		return true
	}
	if err != nil && strings.Contains(err.Error(), "no issue found matching") {
		return true
	}
	return false
}

// RoutedResult contains the result of a routed issue lookup
type RoutedResult struct {
	Issue      *types.Issue
	Store      storage.DoltStorage // The store that contains this issue (may be routed)
	Routed     bool                // true if the issue was found via routing
	ResolvedID string              // The resolved (full) issue ID
	closeFn    func()              // Function to close routed storage (if any)
}

// Close closes any routed storage. Safe to call if Routed is false.
func (r *RoutedResult) Close() {
	if r.closeFn != nil {
		r.closeFn()
	}
}

// resolveAndGetIssueWithRouting resolves a partial ID and gets the issue.
// Tries the local store first, then prefix-based routing via routes.jsonl,
// then falls back to contributor auto-routing.
//
// Returns a RoutedResult containing the issue, resolved ID, and the store to use.
// The caller MUST call result.Close() when done to release any routed storage.
//
// Prefix-routed target stores are opened read-only; mutating commands must use
// resolveAndGetIssueForMutation instead so a routed read can never write
// migrations or other open-time mutations into a foreign project (GH#3231, #4141).
func resolveAndGetIssueWithRouting(ctx context.Context, localStore storage.DoltStorage, id string) (*RoutedResult, error) {
	return resolveAndGetIssueWithRoutingAccess(ctx, localStore, id, false)
}

// resolveAndGetIssueForMutation resolves an issue like
// resolveAndGetIssueWithRouting, but opens prefix-routed target stores in
// writable mode so mutation commands can commit to the routed repository.
func resolveAndGetIssueForMutation(ctx context.Context, localStore storage.DoltStorage, id string) (*RoutedResult, error) {
	return resolveAndGetIssueWithRoutingAccess(ctx, localStore, id, true)
}

func resolveAndGetIssueWithRoutingAccess(ctx context.Context, localStore storage.DoltStorage, id string, writablePrefixRoute bool) (*RoutedResult, error) {
	// Try local store first.
	result, err := resolveAndGetFromStore(ctx, localStore, id, false)
	if err == nil {
		return result, nil
	}

	// If not found locally, try prefix-based routing via routes.jsonl.
	// This handles cross-rig lookups where the ID's prefix maps to a different
	// database (e.g., hr-8wn.1 routes to the herald rig's database).
	if isNotFoundErr(err) {
		if prefixResult, prefixErr := resolveViaPrefixRoutingWithAccess(ctx, id, writablePrefixRoute); prefixErr == nil {
			return prefixResult, nil
		}
	}

	// If not found via prefix routing, try contributor auto-routing as fallback (GH#2345).
	// Auto-routed stores stay read-only even for write-intent callers (writablePrefixRoute):
	// this path hydrates foreign contributor projects, which must never be mutated.
	if isNotFoundErr(err) {
		if autoResult, autoErr := resolveViaAutoRouting(ctx, localStore, id); autoErr == nil {
			return autoResult, nil
		}
	}

	return nil, err
}

// resolveAndGetFromStore resolves a partial ID and gets the issue from a specific store.
func resolveAndGetFromStore(ctx context.Context, s storage.DoltStorage, id string, routed bool) (*RoutedResult, error) {
	// First, resolve the partial ID
	resolvedID, err := utils.ResolvePartialID(ctx, s, id)
	if err != nil {
		return nil, err
	}

	// Then get the issue
	issue, err := s.GetIssue(ctx, resolvedID)
	if err != nil {
		return nil, err
	}

	return &RoutedResult{
		Issue:      issue,
		Store:      s,
		Routed:     routed,
		ResolvedID: resolvedID,
	}, nil
}

// resolveViaAutoRouting attempts to find an issue using contributor auto-routing.
// This is the fallback when the local store doesn't have the issue (GH#2345).
// Returns a RoutedResult if the issue is found in the auto-routed store.
func resolveViaAutoRouting(ctx context.Context, localStore storage.DoltStorage, id string) (*RoutedResult, error) {
	routedStore, routed, _, err := openRoutedReadStore(ctx, localStore)
	if err != nil || !routed {
		return nil, fmt.Errorf("no auto-routed store available")
	}

	result, err := resolveAndGetFromStore(ctx, routedStore, id, true)
	if err != nil {
		_ = routedStore.Close()
		return nil, err
	}
	result.closeFn = func() { _ = routedStore.Close() }
	return result, nil
}

// prefixRoute represents a prefix-to-path routing rule from routes.jsonl.
type prefixRoute struct {
	Prefix string `json:"prefix"` // Issue ID prefix (e.g., "hr-")
	Path   string `json:"path"`   // Relative path to rig directory from town root
}

// resolveViaPrefixRouting attempts to find an issue by looking up its prefix
// in routes.jsonl and opening the target rig's database read-only.
//
// This enables cross-rig lookups: when running from a redirected .beads directory
// (e.g., crew/beercan → town/.beads with database "hq"), a bead ID like "hr-8wn.1"
// can be resolved by following the "hr-" route to the herald rig's .beads directory,
// which declares dolt_database="herald".
//
// The read-only open guarantees a routed read cannot mutate the target; mutation
// commands must route through resolveViaPrefixRoutingWithAccess with writable=true.
func resolveViaPrefixRouting(ctx context.Context, id string) (*RoutedResult, error) {
	return resolveViaPrefixRoutingWithAccess(ctx, id, false)
}

// resolveViaPrefixRoutingWithAccess is the shared implementation that selects the
// store-open mode. writable opens the routed target writable, behaving like running
// the command inside that rig; false keeps the read-only open that guarantees a
// routed read cannot mutate the target (bd-6dnrw.32).
func resolveViaPrefixRoutingWithAccess(ctx context.Context, id string, writable bool) (*RoutedResult, error) {
	route, townRoot, err := findPrefixRoute(id)
	if err != nil {
		return nil, err
	}
	targetStore, err := openPrefixRoute(ctx, route, townRoot, writable)
	if err != nil {
		return nil, err
	}

	result, err := resolveAndGetFromStore(ctx, targetStore, id, true)
	if err != nil {
		_ = targetStore.Close()
		return nil, err
	}
	result.closeFn = func() { _ = targetStore.Close() }

	if os.Getenv("BD_DEBUG_ROUTING") != "" {
		fmt.Fprintf(os.Stderr, "[routing] Resolved %s via prefix route to %s\n", id, route.Path)
	}

	return result, nil
}

// findPrefixRoute returns the routes.jsonl entry for id, read from the
// current beads directory, and the town root its path is relative to. A route
// back to the current database (path ".") is an error: the caller has
// already looked there.
func findPrefixRoute(id string) (*prefixRoute, string, error) {
	if strings.Index(id, "-") <= 0 {
		return nil, "", fmt.Errorf("no prefix in ID %q", id)
	}

	// Find the resolved beads directory (where routes.jsonl lives)
	currentBeadsDir := resolveCommandBeadsDir(dbPath)
	if currentBeadsDir == "" {
		return nil, "", fmt.Errorf("no beads directory available")
	}

	// Load routes from routes.jsonl
	routes, err := loadPrefixRoutes(currentBeadsDir)
	if err != nil || len(routes) == 0 {
		return nil, "", fmt.Errorf("no routes available")
	}

	// Find the most specific route for this ID
	matchedRoute := matchPrefixRoute(routes, id)
	if matchedRoute == nil {
		return nil, "", fmt.Errorf("no route for ID %q", id)
	}

	// Skip if the route points to current directory (town-level, already checked)
	if matchedRoute.Path == "." {
		return nil, "", fmt.Errorf("route points to current database")
	}

	// Derive the town root from the current beads dir.
	// currentBeadsDir is typically <town_root>/.beads
	return matchedRoute, filepath.Dir(currentBeadsDir), nil
}

// openPrefixRoute opens the database a route points at. Read-only unless
// writable, so a routed read can never mutate the target.
func openPrefixRoute(ctx context.Context, route *prefixRoute, townRoot string, writable bool) (storage.DoltStorage, error) {
	// Resolve the target rig's .beads directory
	rigDir := filepath.Join(townRoot, route.Path)
	targetBeadsDir := beads.FollowRedirect(filepath.Join(rigDir, ".beads"))

	// Check that the target has a different dolt_database
	targetDB := readDoltDatabase(targetBeadsDir)
	if targetDB == "" {
		return nil, fmt.Errorf("target rig has no dolt_database configured")
	}

	debug.Logf("[routing] Prefix %q matched route to %s (database: %s)\n", route.Prefix, route.Path, targetDB)

	// We need to temporarily override BEADS_DOLT_SERVER_DATABASE so server-mode
	// stores connect to the correct database on the shared Dolt server.
	origDB := os.Getenv("BEADS_DOLT_SERVER_DATABASE")
	_ = os.Setenv("BEADS_DOLT_SERVER_DATABASE", targetDB)
	var targetStore storage.DoltStorage
	var openErr error
	if writable {
		targetStore, openErr = newDoltStoreFromConfig(ctx, targetBeadsDir)
	} else {
		targetStore, openErr = newReadOnlyStoreFromConfig(ctx, targetBeadsDir)
	}
	// Restore the original env var
	if origDB != "" {
		_ = os.Setenv("BEADS_DOLT_SERVER_DATABASE", origDB)
	} else {
		_ = os.Unsetenv("BEADS_DOLT_SERVER_DATABASE")
	}
	if openErr != nil {
		return nil, fmt.Errorf("opening routed store for %s: %w", route.Path, openErr)
	}
	return targetStore, nil
}

// routedBlockerTimeout is the context deadline for one routed database lookup.
// A lock timeout leaves the blocker unresolved and blocking.
const routedBlockerTimeout = 5 * time.Second

// lookupRoutedBlockers reads cross-prefix blocker issues from the databases
// routes.jsonl assigns them to, opening each database once, read-only. It is
// the externaldeps.IssueRouter behind single-process bd ready and bd blocked.
func lookupRoutedBlockers(ctx context.Context, ids []string) (map[string]*types.Issue, map[string]error) {
	found := make(map[string]*types.Issue, len(ids))
	failed := make(map[string]error)
	type group struct {
		route    *prefixRoute
		townRoot string
		ids      []string
	}
	groups := make(map[string]*group)
	var order []string
	for _, id := range ids {
		route, townRoot, err := findPrefixRoute(id)
		if err != nil {
			failed[id] = err
			continue
		}
		key := filepath.Join(townRoot, route.Path)
		if groups[key] == nil {
			groups[key] = &group{route: route, townRoot: townRoot}
			order = append(order, key)
		}
		groups[key].ids = append(groups[key].ids, id)
	}
	for _, key := range order {
		g := groups[key]
		err := func() error {
			routeCtx, cancel := context.WithTimeout(ctx, routedBlockerTimeout)
			defer cancel()
			target, err := openPrefixRoute(routeCtx, g.route, g.townRoot, false)
			if err != nil {
				return timeoutHint(routeCtx, err)
			}
			defer func() { _ = target.Close() }()
			issues, err := target.GetIssuesByIDs(routeCtx, g.ids)
			if err != nil {
				return timeoutHint(routeCtx, err)
			}
			for _, issue := range issues {
				if issue != nil {
					found[issue.ID] = issue
				}
			}
			return nil
		}()
		for _, id := range g.ids {
			if err != nil {
				failed[id] = err
			} else if found[id] == nil {
				failed[id] = fmt.Errorf("not found in %s", g.route.Path)
			}
		}
	}
	return found, failed
}

// matchPrefixRoute returns the route whose prefix is the longest leading match
// for beadID, or nil if none matches. Route prefixes end in "-", so a
// multi-hyphen prefix such as "claude-os-" matches "claude-os-76l", and with
// both "hq-" and "hq-cv-" configured, "hq-cv-abc" takes the "hq-cv-" route
// (GH#5048).
func matchPrefixRoute(routes []prefixRoute, beadID string) *prefixRoute {
	var best *prefixRoute
	for i, r := range routes {
		if !strings.HasSuffix(r.Prefix, "-") || !strings.HasPrefix(beadID, r.Prefix) {
			continue
		}
		if best == nil || len(r.Prefix) > len(best.Prefix) {
			best = &routes[i]
		}
	}
	return best
}

// loadPrefixRoutes loads prefix-to-path routes from routes.jsonl in the beads directory.
func loadPrefixRoutes(beadsDir string) ([]prefixRoute, error) {
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	file, err := os.Open(routesPath) //nolint:gosec // G304: path is constructed from trusted beads directory
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var routes []prefixRoute
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var route prefixRoute
		if err := json.Unmarshal([]byte(line), &route); err != nil {
			continue
		}
		if route.Prefix != "" && route.Path != "" {
			routes = append(routes, route)
		}
	}
	return routes, scanner.Err()
}

// readDoltDatabase reads the dolt_database field from a .beads/metadata.json file.
func readDoltDatabase(beadsDir string) string {
	metadataPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metadataPath) //nolint:gosec // G304: path is constructed from trusted beads directory
	if err != nil {
		return ""
	}
	var meta struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if json.Unmarshal(data, &meta) != nil {
		return ""
	}
	return meta.DoltDatabase
}

// getIssueWithRouting gets an issue by exact ID.
// Tries the local store first, then prefix-based routing, then contributor auto-routing.
//
// Returns a RoutedResult containing the issue and the store to use for related queries.
// The caller MUST call result.Close() when done to release any routed storage.
func getIssueWithRouting(ctx context.Context, localStore storage.DoltStorage, id string) (*RoutedResult, error) {
	// Try local store first.
	issue, err := localStore.GetIssue(ctx, id)
	if err == nil {
		return &RoutedResult{
			Issue:      issue,
			Store:      localStore,
			Routed:     false,
			ResolvedID: id,
		}, nil
	}

	// If not found locally, try prefix-based routing via routes.jsonl.
	if isNotFoundErr(err) {
		if prefixResult, prefixErr := resolveViaPrefixRouting(ctx, id); prefixErr == nil {
			return prefixResult, nil
		}
	}

	// If not found via prefix routing, try contributor auto-routing as fallback (GH#2345).
	if isNotFoundErr(err) {
		if autoResult, autoErr := resolveViaAutoRouting(ctx, localStore, id); autoErr == nil {
			return autoResult, nil
		}
	}

	return nil, err
}

func timeoutHint(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("gave up after %s, database busy or locked by another process: %w", routedBlockerTimeout, err)
	}
	return err
}
