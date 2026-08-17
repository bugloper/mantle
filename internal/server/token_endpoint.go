package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/auth/identity"
	"github.com/mantle-sh/mantle/internal/auth/token"
	"github.com/mantle-sh/mantle/internal/catalog"
	ocierrors "github.com/mantle-sh/mantle/internal/distribution/errors"
	"github.com/mantle-sh/mantle/internal/observability"
)

// TokenEndpoint implements the Docker registry token service (§9.1).
type TokenEndpoint struct {
	issuer        *token.Issuer
	identities    *identity.Store
	catalog       *catalog.Store
	metrics       *observability.Metrics
	logger        *slog.Logger
	realm         string
	anonymousPull bool
}

// NewTokenEndpoint builds the token service.
func NewTokenEndpoint(issuer *token.Issuer, identities *identity.Store, cat *catalog.Store,
	metrics *observability.Metrics, logger *slog.Logger, realm string, anonymousPull bool) *TokenEndpoint {
	return &TokenEndpoint{
		issuer: issuer, identities: identities, catalog: cat,
		metrics: metrics, logger: logger, realm: realm, anonymousPull: anonymousPull,
	}
}

// tokenResponse is the body clients expect. Both `token` and `access_token`
// carry the same value: the original Docker specification named the field
// `token`, OAuth2 named it `access_token`, and different clients read different
// ones. Emitting both is the only way to satisfy all of them.
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// ServeHTTP issues a scoped registry token.
func (t *TokenEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		ocierrors.ServeJSON(w, ocierrors.Unsupported("the token endpoint accepts GET and POST"), nil)
		return
	}

	// This endpoint has two shapes in the wild, and a registry has to serve
	// both. The original Docker form is a GET carrying the scope in the query
	// string and the credential in a Basic header. The OAuth2 form is a POST
	// carrying grant_type, service, scope and username/password in a
	// form-encoded body — which is what containerd, and therefore Docker 29,
	// sends when pushing.
	//
	// Reading only the query string is silently catastrophic rather than
	// merely wrong: the request still authenticates as nobody and still
	// succeeds, so the client is handed a 200 and a well-formed token granting
	// nothing at all. It then retries the push against that token forever. The
	// failure surfaces as a 401 loop on the blob endpoint with no indication
	// that the token service was ever involved.
	query := r.URL.Query()
	scopes := query["scope"]
	formUsername, formPassword := "", ""
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			if formScopes := r.PostForm["scope"]; len(formScopes) > 0 {
				scopes = formScopes
			}
			formUsername = r.PostForm.Get("username")
			formPassword = r.PostForm.Get("password")
		}
	}
	requested := authz.ParseScopes(scopes)

	// Authenticate. An unauthenticated request is permitted and yields a token
	// with no access, which is how anonymous pull of a public image works: the
	// client still presents a token, it simply grants nothing beyond what
	// public visibility already allows.
	username, password, ok := r.BasicAuth()
	if !ok && formUsername != "" {
		// The OAuth2 password grant. Credentials in a body rather than a
		// header are equivalent for our purposes — the transport is the same,
		// and both are protected only by TLS.
		username, password, ok = formUsername, formPassword, true
	}

	var actor *identity.Identity
	if ok {
		resolved, err := t.identities.Authenticate(r.Context(), username, password)
		if err != nil {
			t.countFailure("bad_credentials")
			t.logger.Info("token request failed authentication",
				"username", username,
				"address", clientIP(r),
				"reason", err)
			// The challenge is repeated so a client that guessed the wrong
			// credential type can retry, and the delay is uniform whether the
			// principal exists or not (SEC-08).
			w.Header().Set("WWW-Authenticate", `Basic realm="mantle"`)
			ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeUnauthorized,
				"invalid username or password"), nil)
			return
		}
		actor = resolved
	}

	if actor == nil && !t.anonymousPull && len(requested) > 0 {
		w.Header().Set("WWW-Authenticate", `Basic realm="mantle"`)
		ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeUnauthorized,
			"authentication required"), nil)
		return
	}

	granted := t.grantScopes(r, actor, requested)

	subject := "mantle:anonymous:anonymous"
	kind := "anonymous"
	if actor != nil {
		subject = actor.Subject()
		kind = string(actor.Kind)
	}

	issued, err := t.issuer.Issue(token.IssueParams{
		Subject: subject,
		Kind:    kind,
		Access:  token.AccessFromScopes(granted),
	})
	if err != nil {
		t.logger.Error("issuing registry token", "error", err)
		ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeUnknown,
			"could not issue a token"), nil)
		return
	}

	if t.metrics != nil {
		t.metrics.TokensIssued.Inc()
	}
	t.logger.Debug("issued registry token",
		"subject", subject, "scopes", len(granted), "expires_in", issued.ExpiresIn)

	body, err := json.Marshal(tokenResponse{
		Token:       issued.Token,
		AccessToken: issued.Token,
		ExpiresIn:   issued.ExpiresIn,
		IssuedAt:    issued.IssuedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeUnknown, "could not encode the token"), nil)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A token must never be cached by an intermediary.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// grantScopes narrows each requested scope to what the principal holds (§9.1).
//
// A partial grant is never an error. Docker requests `pull,push` on operations
// that only need `pull`, so refusing to issue a token for a scope the caller
// cannot fully have would break ordinary pulls.
func (t *TokenEndpoint) grantScopes(r *http.Request, actor *identity.Identity, requested []authz.Scope) []authz.Scope {
	granted := make([]authz.Scope, 0, len(requested))

	for _, scope := range requested {
		switch scope.Type {
		case "repository":
			permissions := t.repositoryPermissions(r, actor, scope.Name)
			narrowed := authz.Intersect(scope, permissions)
			if len(narrowed.Actions) > 0 {
				granted = append(granted, narrowed)
			}

		case "registry":
			// registry:catalog:* is the only registry-scoped resource Mantle
			// recognises. The catalog handler filters by permission anyway, so
			// granting the scope to any authenticated caller is safe and
			// avoids a pointless second round trip.
			if actor != nil {
				granted = append(granted, scope)
			}

		default:
			// Unknown resource types are dropped rather than rejected, so a
			// client asking for something we do not implement still gets a
			// usable token for the scopes we do.
		}
	}
	return granted
}

// repositoryPermissions resolves what the principal may do to a repository,
// including the public-visibility case for anonymous callers.
func (t *TokenEndpoint) repositoryPermissions(r *http.Request, actor *identity.Identity, name string) authz.Permissions {
	permissions := authz.Permissions{}

	repo, err := t.catalog.RepositoryByName(r.Context(), name)
	if err == nil && repo.IsPublic() {
		if actor != nil || t.anonymousPull {
			permissions[authz.ActionPull] = true
		}
	}
	if actor == nil {
		return permissions
	}
	if actor.InstanceAdmin {
		for _, action := range authz.RoleOwner.Actions() {
			permissions[action] = true
		}
		return permissions
	}

	var resolved authz.Permissions
	if err == nil {
		resolved, err = t.identities.PermissionsForRepository(r.Context(), actor.ID, repo.ID)
	} else {
		// The repository does not exist yet; authorise against the name so a
		// namespace grant permits the first push to create it.
		resolved, err = t.identities.PermissionsForName(r.Context(), actor.ID, name)
	}
	if err != nil {
		t.logger.Error("resolving permissions for token scope", "repository", name, "error", err)
		return permissions
	}
	for action := range resolved {
		permissions[action] = true
	}
	return permissions
}

func (t *TokenEndpoint) countFailure(reason string) {
	if t.metrics != nil {
		t.metrics.AuthFailures.WithLabelValues(reason).Inc()
	}
}

// JWKSHandler publishes the token signing key's public half, so that a future
// federated deployment can verify Mantle's tokens without holding its key.
func JWKSHandler(issuer *token.Issuer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := json.Marshal(issuer.Key().PublicJWKS())
		if err != nil {
			ocierrors.ServeJSON(w, ocierrors.New(ocierrors.CodeUnknown, "could not encode the key set"), nil)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(body)
	})
}
