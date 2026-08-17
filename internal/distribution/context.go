package distribution

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/auth/identity"
	"github.com/mantle-sh/mantle/internal/auth/token"
	"github.com/mantle-sh/mantle/internal/catalog"
)

type ctxKey int

const (
	endpointClassKey ctxKey = iota
	authContextKey
)

// setEndpointClass records which route matched, so the metrics middleware can
// label the request without re-deriving it from the path.
func setEndpointClass(r *http.Request, class string) {
	*r = *r.WithContext(context.WithValue(r.Context(), endpointClassKey, class))
}

// EndpointClass returns the matched route's name, or "unknown".
func EndpointClass(ctx context.Context) string {
	if class, ok := ctx.Value(endpointClassKey).(string); ok {
		return class
	}
	return "unknown"
}

// authContext is the outcome of authenticating one request.
type authContext struct {
	// Identity is nil for an anonymous request.
	Identity *identity.Identity
	// Claims is the verified token, nil when no token was presented.
	Claims *token.Claims
	// Anonymous reports that no credential was presented at all.
	Anonymous bool
}

// IdentityID returns the acting identity's id, or nil when anonymous.
func (a *authContext) IdentityID() *int64 {
	if a == nil || a.Identity == nil {
		return nil
	}
	id := a.Identity.ID
	return &id
}

// Name returns a display name for logs and audit records.
func (a *authContext) Name() string {
	if a == nil || a.Identity == nil {
		return "anonymous"
	}
	return a.Identity.Name
}

// IsInstanceAdmin reports instance administrator status.
func (a *authContext) IsInstanceAdmin() bool {
	return a != nil && a.Identity != nil && a.Identity.InstanceAdmin
}

func withAuth(r *http.Request, a *authContext) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authContextKey, a))
}

func authFrom(ctx context.Context) *authContext {
	if a, ok := ctx.Value(authContextKey).(*authContext); ok {
		return a
	}
	return &authContext{Anonymous: true}
}

// visibilityFilter builds the catalog visibility filter for the caller.
func (a *authContext) visibilityFilter(anonymousPull bool) catalog.VisibilityFilter {
	filter := catalog.VisibilityFilter{IncludePublic: anonymousPull}
	if a == nil || a.Identity == nil {
		return filter
	}
	if a.Identity.InstanceAdmin {
		filter.All = true
		return filter
	}
	filter.IdentityID = &a.Identity.ID
	// An authenticated caller can always see public repositories, regardless of
	// whether anonymous pull is enabled — that setting governs unauthenticated
	// access, not what "public" means.
	filter.IncludePublic = true
	return filter
}

// urlPathUnescape decodes a percent-encoded path component and rejects an
// encoded separator.
//
// An encoded slash is refused rather than decoded: the router has already
// decided where the component boundaries are, and letting a decoded "%2F"
// introduce a new one would mean the name Mantle stores is not the name the
// router matched.
func urlPathUnescape(s string) (string, error) {
	decoded, err := url.PathUnescape(s)
	if err != nil {
		return "", err
	}
	if strings.Contains(decoded, "/") && !strings.Contains(s, "/") {
		return "", errEncodedSeparator
	}
	return decoded, nil
}

type encodedSeparatorError struct{}

func (encodedSeparatorError) Error() string {
	return "percent-encoded path separator is not permitted"
}

var errEncodedSeparator = encodedSeparatorError{}

// requiredAction maps an HTTP method on the /v2 surface to the permission it
// needs. Keeping this in one function means a new handler cannot accidentally
// ship without an authorization requirement.
func requiredAction(method, endpointClass string) authz.Action {
	// Everything under /blobs/uploads/ is part of pushing, whatever the method.
	// A GET there is end-13, an upload's status, and a DELETE is cancelling
	// one's own upload — neither is a read, and neither is a deletion.
	if endpointClass == "upload_session" || endpointClass == "upload_start" {
		return authz.ActionPush
	}

	switch method {
	case http.MethodGet, http.MethodHead:
		return authz.ActionPull
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return authz.ActionPush
	case http.MethodDelete:
		switch endpointClass {
		case "manifest", "blob":
			return authz.ActionDeleteTag
		default:
			return authz.ActionPush
		}
	default:
		return authz.ActionPull
	}
}

// scopeActionFor maps an internal action to the token scope action a client
// would have requested for it.
func scopeActionFor(action authz.Action) string {
	switch action {
	case authz.ActionPush:
		return "push"
	case authz.ActionDeleteTag, authz.ActionDeleteRepo:
		return "delete"
	default:
		return "pull"
	}
}
