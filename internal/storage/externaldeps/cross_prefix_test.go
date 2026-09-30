package externaldeps

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// routedStore builds a decorator whose issue router answers from foreign,
// recording each call's ids. failed, when set, is returned as the router's
// per-id errors.
func routedStore(raw *fakeStore, foreign map[string]*types.Issue, failed map[string]error, calls *[][]string) *Store {
	return New(raw, nil, nil, WithIssueRouter(func(_ context.Context, ids []string) (map[string]*types.Issue, map[string]error) {
		if calls != nil {
			*calls = append(*calls, slices.Clone(ids))
		}
		found := make(map[string]*types.Issue)
		for _, id := range ids {
			if issue := foreign[id]; issue != nil {
				found[id] = issue
			}
		}
		return found, failed
	}))
}

func foreignIssue(id string, status types.Status) *types.Issue {
	return &types.Issue{ID: id, Title: id, Status: status, IssueType: types.TypeTask}
}

func TestGetReadyWorkBlocksOnOpenCrossPrefixIssue(t *testing.T) {
	a, b := issue("be-a"), issue("be-b")
	raw := &fakeStore{
		ready: []*types.Issue{a, b},
		deps: map[string][]*types.Dependency{
			a.ID: {externalDep(a.ID, "other-1", types.DepBlocks)},
		},
	}
	store := routedStore(raw, map[string]*types.Issue{"other-1": foreignIssue("other-1", types.StatusOpen)}, nil, nil)

	got, err := store.GetReadyWork(t.Context(), types.WorkFilter{})
	if err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	if ids := issueIDs(got); !slices.Equal(ids, []string{"be-b"}) {
		t.Fatalf("ready IDs = %v, want [be-b]", ids)
	}
}

func TestGetReadyWorkReleasesClosedOrPinnedCrossPrefixIssue(t *testing.T) {
	a, b, c := issue("be-a"), issue("be-b"), issue("be-c")
	raw := &fakeStore{
		ready: []*types.Issue{a, b, c},
		deps: map[string][]*types.Dependency{
			a.ID: {externalDep(a.ID, "other-closed", types.DepBlocks)},
			b.ID: {externalDep(b.ID, "other-pinned", types.DepBlocks)},
			c.ID: {externalDep(c.ID, "other-open", types.DepRelated)},
		},
	}
	store := routedStore(raw, map[string]*types.Issue{
		"other-closed": foreignIssue("other-closed", types.StatusClosed),
		"other-pinned": foreignIssue("other-pinned", types.StatusPinned),
		"other-open":   foreignIssue("other-open", types.StatusOpen),
	}, nil, nil)

	got, err := store.GetReadyWork(t.Context(), types.WorkFilter{})
	if err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	if ids := issueIDs(got); !slices.Equal(ids, []string{"be-a", "be-b", "be-c"}) {
		t.Fatalf("ready IDs = %v, want [be-a be-b be-c]", ids)
	}
}

func TestGetReadyWorkFailsClosedForUnresolvedCrossPrefixIssue(t *testing.T) {
	a, b, c := issue("be-a"), issue("be-b"), issue("be-c")
	raw := &fakeStore{
		ready: []*types.Issue{a, b, c},
		deps: map[string][]*types.Dependency{
			a.ID: {externalDep(a.ID, "noroute-1", types.DepBlocks)},
			b.ID: {externalDep(b.ID, "missing-1", types.DepBlocks)},
		},
	}
	store := routedStore(raw, nil, map[string]error{"noroute-1": errors.New(`no route for ID "noroute-1"`)}, nil)
	warned := map[string]string{}
	store.warnIssue = func(id string, err error) { warned[id] = err.Error() }

	got, err := store.GetReadyWork(t.Context(), types.WorkFilter{})
	if err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	if ids := issueIDs(got); !slices.Equal(ids, []string{"be-c"}) {
		t.Fatalf("ready IDs = %v, want [be-c]", ids)
	}
	want := map[string]string{"noroute-1": `no route for ID "noroute-1"`, "missing-1": "issue not found"}
	if len(warned) != len(want) || warned["noroute-1"] != want["noroute-1"] || warned["missing-1"] != want["missing-1"] {
		t.Fatalf("warnings = %v, want %v", warned, want)
	}
}

func TestGetReadyWorkFailsClosedWithoutIssueRouter(t *testing.T) {
	a := issue("be-a")
	raw := &fakeStore{
		ready: []*types.Issue{a},
		deps: map[string][]*types.Dependency{
			a.ID: {externalDep(a.ID, "other-1", types.DepBlocks)},
		},
	}
	store := New(raw, nil, nil)
	var warned []string
	store.warnIssue = func(id string, _ error) { warned = append(warned, id) }

	for range 2 {
		got, err := store.GetReadyWork(t.Context(), types.WorkFilter{})
		if err != nil {
			t.Fatalf("GetReadyWork: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ready = %v, want no issues", issueIDs(got))
		}
	}
	if !slices.Equal(warned, []string{"other-1"}) {
		t.Fatalf("warnings = %v, want one for other-1", warned)
	}
}

func TestGetReadyWorkRoutesEachCrossPrefixIssueOnce(t *testing.T) {
	a, b := issue("be-a"), issue("be-b")
	raw := &fakeStore{
		ready: []*types.Issue{a, b},
		deps: map[string][]*types.Dependency{
			a.ID: {externalDep(a.ID, "other-1", types.DepBlocks)},
			b.ID: {externalDep(b.ID, "other-1", types.DepBlocks), externalDep(b.ID, "other-2", types.DepBlocks)},
		},
	}
	var calls [][]string
	store := routedStore(raw, map[string]*types.Issue{
		"other-1": foreignIssue("other-1", types.StatusOpen),
		"other-2": foreignIssue("other-2", types.StatusOpen),
	}, nil, &calls)

	if _, err := store.GetReadyWork(t.Context(), types.WorkFilter{}); err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("router calls = %v, want one batched call", calls)
	}
	got := slices.Clone(calls[0])
	slices.Sort(got)
	if !slices.Equal(got, []string{"other-1", "other-2"}) {
		t.Fatalf("routed ids = %v, want [other-1 other-2]", got)
	}
}

func TestSamePrefixDependencyIsLeftToLocalStore(t *testing.T) {
	a := issue("be-a")
	raw := &fakeStore{
		ready: []*types.Issue{a},
		deps: map[string][]*types.Dependency{
			// The fallback record source returns local edges too; a same-prefix
			// target is the local store's business, not this decorator's.
			a.ID: {externalDep(a.ID, "be-gone", types.DepBlocks)},
		},
	}
	var calls [][]string
	store := routedStore(raw, nil, nil, &calls)

	got, err := store.GetReadyWork(t.Context(), types.WorkFilter{})
	if err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	if ids := issueIDs(got); !slices.Equal(ids, []string{"be-a"}) || len(calls) != 0 {
		t.Fatalf("ready IDs = %v, router calls = %v; want [be-a] and no calls", ids, calls)
	}
}

func TestBlockedIncludesOpenCrossPrefixIssue(t *testing.T) {
	a, b := issue("be-a"), issue("be-b")
	raw := &fakeStore{ready: []*types.Issue{a, b}, deps: map[string][]*types.Dependency{
		a.ID: {externalDep(a.ID, "other-1", types.DepBlocks)},
	}}
	store := routedStore(raw, map[string]*types.Issue{"other-1": foreignIssue("other-1", types.StatusOpen)}, nil, nil)

	blockedList, err := store.GetBlockedIssues(t.Context(), types.WorkFilter{})
	if err != nil {
		t.Fatalf("GetBlockedIssues: %v", err)
	}
	if ids := blockedIssueIDs(blockedList); !slices.Equal(ids, []string{a.ID}) || !slices.Equal(blockedList[0].BlockedBy, []string{"other-1"}) {
		t.Fatalf("blocked = %v (by %v), want [%s] by [other-1]", ids, blockedList, a.ID)
	}
}

func TestOtherCrossPrefixEdgeTypesKeepLocalBehavior(t *testing.T) {
	a, b := issue("be-a"), issue("be-b")
	raw := &fakeStore{ready: []*types.Issue{a, b}, deps: map[string][]*types.Dependency{
		a.ID: {externalDep(a.ID, "other-wait", types.DepWaitsFor)},
		b.ID: {externalDep(b.ID, "other-conditional", types.DepConditionalBlocks)},
	}}
	var calls [][]string
	store := routedStore(raw, nil, nil, &calls)
	ready, err := store.GetReadyWork(t.Context(), types.WorkFilter{})
	if err != nil || !slices.Equal(issueIDs(ready), []string{a.ID, b.ID}) || len(calls) != 0 {
		t.Fatalf("ready=%v, calls=%v, err=%v", issueIDs(ready), calls, err)
	}
}
