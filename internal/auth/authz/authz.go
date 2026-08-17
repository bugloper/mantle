// Package authz implements Mantle's permission model (§9.3): roles, scopes, and
// deny-by-default evaluation.
//
// Two rules govern everything in this package. The effective permission is the
// union of all allow grants matching the principal, intersected with any
// explicit deny — and evaluation happens per request, never per session
// (REQ-AUTHZ-02), so a revoked permission takes effect within one token TTL
// with no exception for work already in flight.
package authz

import (
	"fmt"
	"sort"
	"strings"
)

// Action is a single permission.
type Action string

const (
	ActionPull           Action = "pull"
	ActionPush           Action = "push"
	ActionDeleteTag      Action = "delete_tag"
	ActionDeleteRepo     Action = "delete_repo"
	ActionManageTokens   Action = "manage_tokens"
	ActionManageMembers  Action = "manage_members"
	ActionManagePolicies Action = "manage_policies"
	ActionInstanceAdmin  Action = "instance_admin"
)

// Role is a named bundle of actions.
type Role string

const (
	RoleReader      Role = "reader"
	RoleContributor Role = "contributor"
	RoleMaintainer  Role = "maintainer"
	RoleOwner       Role = "owner"
	RoleOrgAdmin    Role = "org_admin"
)

// roleActions is the §9.3 permission table, transcribed. It is the single
// authority on what a role may do; nothing else in the codebase should decide
// that a maintainer can delete a tag.
var roleActions = map[Role]map[Action]bool{
	RoleReader: {
		ActionPull: true,
	},
	RoleContributor: {
		ActionPull: true, ActionPush: true,
	},
	RoleMaintainer: {
		ActionPull: true, ActionPush: true, ActionDeleteTag: true,
		ActionManageTokens: true, ActionManagePolicies: true,
	},
	RoleOwner: {
		ActionPull: true, ActionPush: true, ActionDeleteTag: true, ActionDeleteRepo: true,
		ActionManageTokens: true, ActionManageMembers: true, ActionManagePolicies: true,
	},
	RoleOrgAdmin: {
		ActionPull: true, ActionPush: true, ActionDeleteTag: true, ActionDeleteRepo: true,
		ActionManageTokens: true, ActionManageMembers: true, ActionManagePolicies: true,
	},
}

// ValidRole reports whether a string names a role.
func ValidRole(s string) bool {
	_, ok := roleActions[Role(s)]
	return ok
}

// Allows reports whether a role includes an action.
func (r Role) Allows(action Action) bool { return roleActions[r][action] }

// Actions returns a role's actions, sorted, for display.
func (r Role) Actions() []Action {
	set := roleActions[r]
	actions := make([]Action, 0, len(set))
	for a := range set {
		actions = append(actions, a)
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i] < actions[j] })
	return actions
}

// Permissions is the resolved set of actions a principal holds on one resource.
type Permissions map[Action]bool

// Has reports whether an action is permitted.
func (p Permissions) Has(action Action) bool { return p[action] }

// Grant is one authorization record, as loaded from the database.
type Grant struct {
	Role   Role
	Effect string // "allow" or "deny"
	// ScopeType is instance, organization, namespace, or repository.
	ScopeType string
}

// Resolve computes effective permissions from a set of grants.
//
// Denies are applied after the union of allows, and a deny removes every action
// its role would have granted. That makes a deny at any scope authoritative:
// the point of an explicit deny is to lock a compromised credential out without
// deleting it, and a deny that could be out-voted by a broader allow would not
// do that.
func Resolve(grants []Grant) Permissions {
	permissions := Permissions{}
	for _, g := range grants {
		if g.Effect != "allow" {
			continue
		}
		for action := range roleActions[g.Role] {
			permissions[action] = true
		}
	}
	for _, g := range grants {
		if g.Effect != "deny" {
			continue
		}
		for action := range roleActions[g.Role] {
			delete(permissions, action)
		}
	}
	return permissions
}

// --- Docker registry token scopes ---

// Scope is one entry of a token's access claim: a resource and the actions
// requested or granted over it.
type Scope struct {
	Type    string // "repository" or "registry"
	Name    string
	Actions []string
}

// ParseScope parses one `type:name:action[,action]` scope string.
//
// The name may itself contain colons in principle, so the split is anchored
// from both ends: the first colon ends the type and the last begins the action
// list. Splitting naively on every colon is a real interoperability bug with
// clients that include a port in a scope name.
func ParseScope(raw string) (Scope, error) {
	if raw == "" {
		return Scope{}, fmt.Errorf("empty scope")
	}
	resourceType, rest, found := strings.Cut(raw, ":")
	if !found {
		return Scope{}, fmt.Errorf("scope %q is malformed: expected type:name:actions", raw)
	}
	lastColon := strings.LastIndex(rest, ":")
	if lastColon < 0 {
		return Scope{}, fmt.Errorf("scope %q is malformed: expected type:name:actions", raw)
	}
	name := rest[:lastColon]
	actionList := rest[lastColon+1:]

	if name == "" {
		return Scope{}, fmt.Errorf("scope %q names no resource", raw)
	}

	var actions []string
	for _, a := range strings.Split(actionList, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		actions = append(actions, a)
	}
	return Scope{Type: resourceType, Name: name, Actions: actions}, nil
}

// ParseScopes parses the repeated or space-separated scope parameters a client
// sends to the token endpoint.
//
// Unparseable scopes are skipped rather than rejected. A client that asks for
// something malformed should receive a token granting nothing for it, which is
// the same outcome as asking for something it cannot have — erroring instead
// would break clients that speculatively request scopes they do not need.
func ParseScopes(raw []string) []Scope {
	var scopes []Scope
	for _, entry := range raw {
		for _, field := range strings.Fields(entry) {
			scope, err := ParseScope(field)
			if err != nil {
				continue
			}
			scopes = append(scopes, scope)
		}
	}
	return scopes
}

func (s Scope) String() string {
	return fmt.Sprintf("%s:%s:%s", s.Type, s.Name, strings.Join(s.Actions, ","))
}

// ActionsForScope maps registry scope actions onto internal actions.
//
// The Docker protocol's vocabulary is narrower than Mantle's: it knows pull,
// push, and delete. Everything else is administrative and travels over the
// admin API rather than a registry token.
func ActionsForScope(scopeAction string) []Action {
	switch scopeAction {
	case "pull":
		return []Action{ActionPull}
	case "push":
		// A push implies the ability to read what is already there, and every
		// client that pushes also probes with HEAD first.
		return []Action{ActionPush, ActionPull}
	case "delete":
		return []Action{ActionDeleteTag}
	case "*":
		// Clients request "*" for administrative scopes such as
		// registry:catalog:*. Granting it means granting what the principal
		// already holds, which the intersection below takes care of.
		return []Action{ActionPull, ActionPush, ActionDeleteTag}
	default:
		return nil
	}
}

// Intersect narrows a requested scope to what the principal actually holds
// (§9.1).
//
// This never errors on a partial grant. Docker requests `pull,push`
// speculatively on almost every operation, including plain pulls, so refusing a
// partial grant would break ordinary use. The token reports what was granted
// and the client discovers the rest when it tries.
func Intersect(requested Scope, held Permissions) Scope {
	granted := Scope{Type: requested.Type, Name: requested.Name}
	for _, scopeAction := range requested.Actions {
		internal := ActionsForScope(scopeAction)
		if len(internal) == 0 {
			continue
		}
		permitted := true
		for _, action := range internal {
			if !held.Has(action) {
				permitted = false
				break
			}
		}
		if permitted {
			granted.Actions = append(granted.Actions, scopeAction)
		}
	}
	return granted
}
