// Package externaldeps resolves blocking dependencies whose target lives
// outside the local database: explicit external:<project>:<capability>
// references, and issue IDs whose prefix belongs to another beads database.
package externaldeps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

const externalPrefix = "external:"

func defaultProjectWarning(project ProjectName) {
	fmt.Fprintf(os.Stderr, "Warning: external project %q is unavailable; its capability dependencies remain blocking\n", project)
}

func defaultIssueWarning(id string, err error) {
	fmt.Fprintf(os.Stderr, "Warning: blocking dependency %s could not be resolved (%v); it remains blocking\n", id, err)
}

// ProjectName is the configured name in external_projects.
type ProjectName string

// CapabilityName is the suffix matched by a provides:<capability> label.
type CapabilityName string

// ProjectLocator resolves a configured project name to its workspace root.
// The bool is false when the project is not configured or unavailable.
type ProjectLocator func(ProjectName) (string, bool)

// StoreOpener opens a project workspace as a read-only store.
type StoreOpener func(context.Context, string) (storage.DoltStorage, error)

// IssueRouter looks up issues that live in other beads databases. found holds
// the issues it could read; failed explains ids it could not. An id in
// neither map was not found.
type IssueRouter func(ctx context.Context, ids []string) (found map[string]*types.Issue, failed map[string]error)

// Option configures a Store.
type Option func(*Store)

// WithIssueRouter lets blocking edges on cross-prefix issue IDs resolve
// against the database that owns them. Without a router those edges stay
// blocking.
func WithIssueRouter(router IssueRouter) Option {
	return func(s *Store) { s.routeIssues = router }
}

type reference struct {
	raw        string
	project    ProjectName
	capability CapabilityName
	valid      bool
	// issueID is set when raw is a cross-prefix issue ID rather than an
	// external: reference.
	issueID string
}

func parseReference(raw string) reference {
	ref := reference{raw: raw}
	if !strings.HasPrefix(raw, externalPrefix) {
		ref.issueID = raw
		return ref
	}
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return ref
	}
	ref.project = ProjectName(parts[1])
	ref.capability = CapabilityName(parts[2])
	ref.valid = true
	return ref
}

func isExternalReference(raw string) bool {
	return strings.HasPrefix(raw, externalPrefix)
}

// isForeignTarget reports whether a blocking edge's target has no row in this
// database and must be resolved here: the same rule that stores the edge in
// depends_on_external.
func isForeignTarget(sourceID, targetID string) bool {
	return issueops.IsExternalDepTarget(sourceID, targetID)
}

// resolveReferences returns ref -> satisfied. Every ref starts unsatisfied;
// malformed refs and foreign-store failures therefore fail closed.
func (s *Store) resolveReferences(ctx context.Context, refs []reference) (map[string]bool, error) {
	result := make(map[string]bool, len(refs))
	byProject := make(map[ProjectName]map[CapabilityName][]string)
	var issueIDs []string
	for _, ref := range refs {
		if _, seen := result[ref.raw]; !seen && ref.issueID != "" {
			issueIDs = append(issueIDs, ref.issueID)
		}
		result[ref.raw] = false
		if !ref.valid {
			continue
		}
		if byProject[ref.project] == nil {
			byProject[ref.project] = make(map[CapabilityName][]string)
		}
		byProject[ref.project][ref.capability] = append(byProject[ref.project][ref.capability], ref.raw)
	}

	if err := s.resolveIssueReferences(ctx, issueIDs, result); err != nil {
		return nil, err
	}

	if s.locateProject == nil || s.openProject == nil {
		return result, nil
	}

	for project, capabilities := range byProject {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, ok := s.locateProject(project)
		if !ok {
			s.warnUnresolvedProject(project)
			continue
		}
		foreign, err := s.openProject(ctx, path)
		if err != nil || foreign == nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			s.warnUnresolvedProject(project)
			continue
		}

		for capability, rawRefs := range capabilities {
			issues, queryErr := foreign.GetIssuesByLabel(ctx, "provides:"+string(capability))
			if queryErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					_ = foreign.Close()
					return nil, ctxErr
				}
				s.warnUnresolvedProject(project)
				continue
			}
			satisfied := false
			for _, issue := range issues {
				if issue != nil && issue.Status == types.StatusClosed {
					satisfied = true
					break
				}
			}
			if satisfied {
				for _, raw := range rawRefs {
					result[raw] = true
				}
			}
		}
		_ = foreign.Close()
	}

	return result, nil
}

// resolveIssueReferences marks cross-prefix issue IDs satisfied when the
// owning database reports them closed or pinned, the statuses that stop a
// local blocker from blocking. An ID that has no route, whose database cannot
// be opened, or that does not exist there stays blocking, with a warning.
func (s *Store) resolveIssueReferences(ctx context.Context, ids []string, result map[string]bool) error {
	if len(ids) == 0 {
		return nil
	}
	if s.routeIssues == nil {
		for _, id := range ids {
			s.warnUnresolvedIssue(id, errors.New("no issue router configured"))
		}
		return nil
	}
	found, failed := s.routeIssues(ctx, ids)
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		issue := found[id]
		if issue == nil {
			err := failed[id]
			if err == nil {
				err = errors.New("issue not found")
			}
			s.warnUnresolvedIssue(id, err)
			continue
		}
		if issue.Status == types.StatusClosed || issue.Status == types.StatusPinned {
			result[id] = true
		}
	}
	return nil
}
