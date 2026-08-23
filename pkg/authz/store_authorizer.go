package authz

import (
	"context"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// StoreAuthorizer evaluates a subject's attached policies out of the
// state store. It is the single policy evaluator in the tree: the API
// server's RBAC interceptor and the orchestrator's admission gate both
// resolve grants through it, so a verb cannot mean one thing on the RPC
// boundary and another at the choke point.
type StoreAuthorizer struct{ store store.Store }

// NewStoreAuthorizer returns an Authorizer reading users and policies
// from st.
func NewStoreAuthorizer(st store.Store) *StoreAuthorizer { return &StoreAuthorizer{store: st} }

// Authorize reports whether the subject holds a policy rule matching
// (resource, verb, namespace). Deny is the default at every step: an
// unknown subject, a subject with no policies, or an unreadable policy
// all fall through to false.
func (a *StoreAuthorizer) Authorize(ctx context.Context, subjectID, resource, verb, namespace string) (bool, error) {
	if a == nil || a.store == nil {
		return false, nil
	}
	// Load user by ID (list and match) as we don't have GetByID.
	var users []types.User
	if err := a.store.List(ctx, types.ResourceTypeUser, "system", &users); err != nil {
		return false, err
	}
	var user *types.User
	for i := range users {
		if users[i].ID == subjectID || users[i].Name == subjectID {
			user = &users[i]
			break
		}
	}
	if user == nil || len(user.Policies) == 0 {
		return false, nil
	}
	pr := repos.NewPolicyRepo(a.store)
	for _, pname := range user.Policies {
		p, err := pr.Get(ctx, pname)
		if err != nil {
			continue
		}
		for _, rule := range p.Rules {
			if rule.Resource != "*" && rule.Resource != resource {
				continue
			}
			verbAllowed := false
			for _, v := range rule.Verbs {
				if v == "*" || v == verb {
					verbAllowed = true
					break
				}
			}
			if !verbAllowed {
				continue
			}
			// Namespace check: empty or "*" on the rule allows any.
			if rule.Namespace == "" || rule.Namespace == "*" || rule.Namespace == namespace {
				return true, nil
			}
		}
	}
	return false, nil
}
