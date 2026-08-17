package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/auth/identity"
	"github.com/mantle-sh/mantle/internal/catalog"
	"github.com/mantle-sh/mantle/internal/oci"
)

// --- organizations ---

type organizationView struct {
	Slug         string `json:"slug"`
	DisplayName  string `json:"display_name"`
	QuotaBytes   *int64 `json:"quota_bytes"`
	UsedBytes    int64  `json:"used_bytes"`
	Repositories int    `json:"repositories"`
}

func (a *API) handleListOrganizations(w http.ResponseWriter, r *http.Request, _ []string, _ *identity.Identity) {
	rows, err := a.opts.Pool.Query(r.Context(), `
		SELECT o.slug::text, o.display_name, o.quota_bytes,
		       (SELECT count(*) FROM repositories rr WHERE rr.organization_id = o.id)
		FROM organizations o ORDER BY o.slug`)
	if err != nil {
		a.internalError(w, r, "listing organizations", err)
		return
	}
	defer rows.Close()

	organizations := []organizationView{}
	for rows.Next() {
		var view organizationView
		if err := rows.Scan(&view.Slug, &view.DisplayName, &view.QuotaBytes, &view.Repositories); err != nil {
			a.internalError(w, r, "listing organizations", err)
			return
		}
		organizations = append(organizations, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"organizations": organizations})
}

func (a *API) handleCreateOrganization(w http.ResponseWriter, r *http.Request, _ []string, _ *identity.Identity) {
	var body struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
		QuotaBytes  *int64 `json:"quota_bytes"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.Slug == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "slug is required")
		return
	}
	if body.DisplayName == "" {
		body.DisplayName = body.Slug
	}

	var id int64
	err := a.opts.Pool.QueryRow(r.Context(), `
		INSERT INTO organizations (slug, display_name, quota_bytes)
		VALUES ($1, $2, $3) RETURNING id`,
		body.Slug, body.DisplayName, body.QuotaBytes).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "already_exists",
				"an organization named "+body.Slug+" already exists")
			return
		}
		a.internalError(w, r, "creating an organization", err)
		return
	}
	writeJSON(w, http.StatusCreated, organizationView{
		Slug: body.Slug, DisplayName: body.DisplayName, QuotaBytes: body.QuotaBytes,
	})
}

// --- repositories ---

type repositoryView struct {
	Name          string    `json:"name"`
	Organization  string    `json:"organization"`
	Visibility    string    `json:"visibility"`
	ImmutableTags bool      `json:"immutable_tags"`
	SourceURL     string    `json:"source_url,omitempty"`
	Tags          int       `json:"tags"`
	Manifests     int       `json:"manifests"`
	UsedBytes     int64     `json:"used_bytes"`
	CreatedAt     time.Time `json:"created_at"`
}

func (a *API) handleListRepositories(w http.ResponseWriter, r *http.Request, _ []string, actor *identity.Identity) {
	filter := catalog.VisibilityFilter{IncludePublic: true}
	if actor != nil {
		if actor.InstanceAdmin {
			filter.All = true
		} else {
			filter.IdentityID = &actor.ID
		}
	}

	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 1000 {
			limit = parsed
		}
	}

	page, err := a.opts.Catalog.ListRepositories(r.Context(), filter, limit, r.URL.Query().Get("after"))
	if err != nil {
		a.internalError(w, r, "listing repositories", err)
		return
	}

	views := make([]repositoryView, 0, len(page.Names))
	for _, name := range page.Names {
		view, err := a.repositoryView(r, name)
		if err != nil {
			continue
		}
		views = append(views, *view)
	}

	response := map[string]any{"repositories": views}
	if page.HasMore && len(page.Names) > 0 {
		response["next"] = page.Names[len(page.Names)-1]
	}
	writeJSON(w, http.StatusOK, response)
}

// handleCreateRepository creates an empty repository.
//
// Pushing to a name creates it automatically, so this is not the primary path —
// it exists so that a repository can be prepared with its visibility and policy
// set *before* the first image lands, rather than existing briefly as a private
// default and being changed afterwards.
func (a *API) handleCreateRepository(w http.ResponseWriter, r *http.Request, _ []string, actor *identity.Identity) {
	var body struct {
		Name          string `json:"name"`
		Organization  string `json:"organization"`
		Visibility    string `json:"visibility"`
		ImmutableTags bool   `json:"immutable_tags"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	// The name is validated against the OCI grammar here, not merely at push
	// time. A repository whose name no client could ever request would be
	// created successfully and then be unusable (REQ-OCI-06).
	if err := oci.ValidateName(body.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", err.Error(),
			"Repository names are lowercase, '/'-separated, e.g. acme/web.")
		return
	}
	if body.Visibility == "" {
		body.Visibility = "private"
	}
	if body.Visibility != "private" && body.Visibility != "public" {
		writeError(w, http.StatusBadRequest, "invalid_body", "visibility must be private or public")
		return
	}

	// The organization is the name's first path component unless one was given.
	organization := body.Organization
	if organization == "" {
		organization, _, _ = strings.Cut(body.Name, "/")
	}
	if organization == "" || !strings.Contains(body.Name, "/") {
		writeError(w, http.StatusBadRequest, "invalid_name",
			"a repository name must be organization-qualified, e.g. acme/web",
			"Create the organization first if it does not exist.")
		return
	}

	var orgID int64
	if err := a.opts.Pool.QueryRow(r.Context(),
		`SELECT id FROM organizations WHERE slug = $1`, organization).Scan(&orgID); err != nil {
		notFound(w, "organization", organization)
		return
	}

	// Creating a repository is a write to the namespace, so it needs push there
	// — the same permission that an implicit create-on-push requires.
	if !actor.InstanceAdmin {
		permissions, err := a.opts.Identities.PermissionsForName(r.Context(), actor.ID, body.Name)
		if err != nil {
			a.internalError(w, r, "evaluating permissions", err)
			return
		}
		if !permissions.Has(authz.ActionPush) {
			// 404 rather than 403, matching REQ-OCI-11: a caller who cannot
			// write to a namespace should not learn what exists in it.
			notFound(w, "organization", organization)
			return
		}
	}

	repo, err := a.opts.Catalog.CreateRepository(r.Context(), orgID, body.Name, body.Visibility)
	if mapStoreError(w, err, "repository", body.Name) {
		return
	}
	if err != nil {
		a.internalError(w, r, "creating a repository", err)
		return
	}

	if body.ImmutableTags {
		if err := a.opts.Catalog.SetImmutableTags(r.Context(), repo.ID, true); err != nil {
			a.internalError(w, r, "enabling tag immutability", err)
			return
		}
	}

	a.opts.Logger.Info("created repository",
		"repository", body.Name, "visibility", body.Visibility,
		"immutable_tags", body.ImmutableTags, "actor", actor.Name)

	view, err := a.repositoryView(r, body.Name)
	if err != nil {
		a.internalError(w, r, "reading the new repository", err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (a *API) repositoryView(r *http.Request, name string) (*repositoryView, error) {
	repo, err := a.opts.Catalog.RepositoryByName(r.Context(), name)
	if err != nil {
		return nil, err
	}
	view := &repositoryView{
		Name:          repo.Name,
		Visibility:    repo.Visibility,
		ImmutableTags: repo.ImmutableTags,
		SourceURL:     repo.SourceURL,
		CreatedAt:     repo.CreatedAt,
	}
	_ = a.opts.Pool.QueryRow(r.Context(),
		`SELECT slug::text FROM organizations WHERE id = $1`, repo.OrganizationID).Scan(&view.Organization)
	_ = a.opts.Pool.QueryRow(r.Context(), `
		SELECT (SELECT count(*) FROM tags WHERE repository_id = $1),
		       (SELECT count(*) FROM manifests WHERE repository_id = $1 AND state = 'available')`,
		repo.ID).Scan(&view.Tags, &view.Manifests)
	view.UsedBytes, _ = a.opts.Catalog.RepositoryUsage(r.Context(), repo.ID)
	return view, nil
}

func (a *API) handleGetRepository(w http.ResponseWriter, r *http.Request, m []string, actor *identity.Identity) {
	name := m[1]
	if !a.mayRead(r, actor, name) {
		notFound(w, "repository", name)
		return
	}
	view, err := a.repositoryView(r, name)
	if mapStoreError(w, err, "repository", name) {
		return
	}
	if err != nil {
		a.internalError(w, r, "reading a repository", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleUpdateRepository(w http.ResponseWriter, r *http.Request, m []string, actor *identity.Identity) {
	name := m[1]
	repo, err := a.opts.Catalog.RepositoryByName(r.Context(), name)
	if mapStoreError(w, err, "repository", name) {
		return
	}
	if err != nil {
		a.internalError(w, r, "reading a repository", err)
		return
	}
	if !a.mayAdminister(r, actor, repo) {
		notFound(w, "repository", name)
		return
	}

	// Pointers distinguish "not supplied" from "set to false", which a partial
	// update must honour — a PATCH that omitted immutable_tags must not clear it.
	var body struct {
		Visibility    *string `json:"visibility"`
		ImmutableTags *bool   `json:"immutable_tags"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	if body.Visibility != nil {
		if err := a.opts.Catalog.SetVisibility(r.Context(), repo.ID, *body.Visibility); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if body.ImmutableTags != nil {
		if err := a.opts.Catalog.SetImmutableTags(r.Context(), repo.ID, *body.ImmutableTags); err != nil {
			a.internalError(w, r, "updating tag immutability", err)
			return
		}
	}

	view, err := a.repositoryView(r, name)
	if err != nil {
		a.internalError(w, r, "reading a repository", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) handleDeleteRepository(w http.ResponseWriter, r *http.Request, m []string, actor *identity.Identity) {
	name := m[1]
	repo, err := a.opts.Catalog.RepositoryByName(r.Context(), name)
	if mapStoreError(w, err, "repository", name) {
		return
	}
	if err != nil {
		a.internalError(w, r, "reading a repository", err)
		return
	}
	if !a.mayAdminister(r, actor, repo) {
		notFound(w, "repository", name)
		return
	}

	err = a.opts.Catalog.DeleteRepository(r.Context(), repo.ID)
	if mapStoreError(w, err, "repository", name) {
		return
	}
	if err != nil {
		a.internalError(w, r, "deleting a repository", err)
		return
	}

	a.opts.Logger.Info("deleted repository", "repository", name, "actor", actor.Name)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": name,
		// Said explicitly, because "I deleted a repository and the disk did not
		// shrink" is otherwise a support thread. Blobs are reclaimed by GC once
		// nothing references them and the quarantine window has passed.
		"note": "storage is reclaimed by garbage collection, not immediately; " +
			"run 'mantle gc run --dry-run' to see what has become collectable",
	})
}

// mayRead reports whether the actor may see a repository.
func (a *API) mayRead(r *http.Request, actor *identity.Identity, name string) bool {
	if actor == nil {
		return false
	}
	if actor.InstanceAdmin {
		return true
	}
	repo, err := a.opts.Catalog.RepositoryByName(r.Context(), name)
	if err != nil {
		return false
	}
	if repo.IsPublic() {
		return true
	}
	permissions, err := a.opts.Identities.PermissionsForRepository(r.Context(), actor.ID, repo.ID)
	if err != nil {
		return false
	}
	return permissions.Has(authz.ActionPull)
}

// mayAdminister reports whether the actor may change or delete a repository.
func (a *API) mayAdminister(r *http.Request, actor *identity.Identity, repo *catalog.Repository) bool {
	if actor == nil {
		return false
	}
	if actor.InstanceAdmin {
		return true
	}
	permissions, err := a.opts.Identities.PermissionsForRepository(r.Context(), actor.ID, repo.ID)
	if err != nil {
		return false
	}
	return permissions.Has(authz.ActionDeleteRepo)
}

// --- users and tokens ---

type identityView struct {
	UUID       string     `json:"uuid"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Email      string     `json:"email,omitempty"`
	Admin      bool       `json:"instance_admin"`
	Disabled   bool       `json:"disabled"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func toIdentityView(i *identity.Identity) identityView {
	return identityView{
		UUID: i.UUID, Kind: string(i.Kind), Name: i.Name, Email: i.Email,
		Admin: i.InstanceAdmin, Disabled: i.Disabled,
		ExpiresAt: i.ExpiresAt, LastUsedAt: i.LastUsedAt, CreatedAt: i.CreatedAt,
	}
}

func (a *API) handleListUsers(w http.ResponseWriter, r *http.Request, _ []string, _ *identity.Identity) {
	users, err := a.opts.Identities.List(r.Context(), identity.KindUser, nil)
	if err != nil {
		a.internalError(w, r, "listing users", err)
		return
	}
	views := make([]identityView, 0, len(users))
	for _, u := range users {
		views = append(views, toIdentityView(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": views})
}

func (a *API) handleCreateUser(w http.ResponseWriter, r *http.Request, _ []string, actor *identity.Identity) {
	var body struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Admin    bool   `json:"instance_admin"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.Name == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "name and password are required")
		return
	}

	created, err := a.opts.Identities.CreateUser(r.Context(), identity.CreateUserParams{
		Name: body.Name, Email: body.Email, Password: body.Password,
		InstanceAdmin: body.Admin, CreatedBy: &actor.ID,
	})
	if mapStoreError(w, err, "user", body.Name) {
		return
	}
	if err != nil {
		a.internalError(w, r, "creating a user", err)
		return
	}
	a.opts.Logger.Info("created user", "user", body.Name, "admin", body.Admin, "actor", actor.Name)
	writeJSON(w, http.StatusCreated, toIdentityView(created))
}

func (a *API) handleListTokens(w http.ResponseWriter, r *http.Request, _ []string, _ *identity.Identity) {
	var views []identityView
	for _, kind := range []identity.Kind{identity.KindPAT, identity.KindRobot, identity.KindDeployToken} {
		found, err := a.opts.Identities.List(r.Context(), kind, nil)
		if err != nil {
			a.internalError(w, r, "listing tokens", err)
			return
		}
		for _, f := range found {
			views = append(views, toIdentityView(f))
		}
	}
	if views == nil {
		views = []identityView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": views})
}

// tokenCreatedView carries the plaintext secret. It is returned exactly once,
// at creation, and is not recoverable afterwards (§9.2).
type tokenCreatedView struct {
	identityView
	Secret string `json:"secret"`
	Notice string `json:"notice"`
}

func (a *API) handleCreateToken(w http.ResponseWriter, r *http.Request, _ []string, actor *identity.Identity) {
	var body struct {
		Name          string `json:"name"`
		Kind          string `json:"kind"`
		Organization  string `json:"organization"`
		Namespace     string `json:"namespace"`
		Role          string `json:"role"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if body.Name == "" || body.Organization == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "name and organization are required")
		return
	}
	kind := identity.Kind(body.Kind)
	switch kind {
	case identity.KindPAT, identity.KindRobot, identity.KindDeployToken:
	case "":
		kind = identity.KindDeployToken
	default:
		writeError(w, http.StatusBadRequest, "invalid_body",
			"kind must be one of pat, robot, deploy_token")
		return
	}
	if body.Role == "" {
		// Pull-only by default. A token that can push when the operator only
		// asked for a token is the wrong default to get wrong.
		body.Role = string(authz.RoleReader)
	}
	if !authz.ValidRole(body.Role) {
		writeError(w, http.StatusBadRequest, "invalid_body",
			"role must be one of reader, contributor, maintainer, owner")
		return
	}

	var orgID int64
	if err := a.opts.Pool.QueryRow(r.Context(),
		`SELECT id FROM organizations WHERE slug = $1`, body.Organization).Scan(&orgID); err != nil {
		notFound(w, "organization", body.Organization)
		return
	}

	var expiresAt *time.Time
	if body.ExpiresInDays > 0 {
		t := time.Now().AddDate(0, 0, body.ExpiresInDays)
		expiresAt = &t
	}

	created, secret, err := a.opts.Identities.CreateMachine(r.Context(), identity.CreateMachineParams{
		Kind: kind, Name: body.Name, OrganizationID: orgID,
		ExpiresAt: expiresAt, CreatedBy: &actor.ID,
	})
	if mapStoreError(w, err, "token", body.Name) {
		return
	}
	if err != nil {
		a.internalError(w, r, "creating a token", err)
		return
	}

	namespace := body.Namespace
	if namespace == "" {
		namespace = body.Organization + "/"
	}
	if err := a.opts.Identities.Grant(r.Context(), identity.GrantParams{
		IdentityID:      &created.ID,
		ScopeType:       "namespace",
		NamespacePrefix: namespace,
		Role:            authz.Role(body.Role),
		Effect:          "allow",
		CreatedBy:       &actor.ID,
	}); err != nil {
		a.internalError(w, r, "granting permissions to a token", err)
		return
	}

	a.opts.Logger.Info("created machine credential",
		"name", body.Name, "kind", kind, "namespace", namespace,
		"role", body.Role, "actor", actor.Name)

	writeJSON(w, http.StatusCreated, tokenCreatedView{
		identityView: toIdentityView(created),
		Secret:       secret,
		Notice:       "This secret is shown once and cannot be retrieved again. Store it now.",
	})
}

func (a *API) handleRevokeToken(w http.ResponseWriter, r *http.Request, m []string, actor *identity.Identity) {
	target, err := a.opts.Identities.ByUUID(r.Context(), m[1])
	if mapStoreError(w, err, "token", m[1]) {
		return
	}
	if err != nil {
		a.internalError(w, r, "reading a token", err)
		return
	}

	// Disabled rather than deleted, so the audit trail that references it
	// survives (§9.3).
	if err := a.opts.Identities.SetDisabled(r.Context(), target.ID, true,
		"revoked by "+actor.Name); err != nil {
		a.internalError(w, r, "revoking a token", err)
		return
	}
	a.opts.Logger.Info("revoked credential", "name", target.Name, "actor", actor.Name)
	writeJSON(w, http.StatusOK, map[string]any{
		"revoked": target.Name,
		"note": "the credential is disabled rather than deleted, so audit records " +
			"referring to it remain meaningful",
	})
}

func (a *API) handleCreateGrant(w http.ResponseWriter, r *http.Request, _ []string, actor *identity.Identity) {
	var body struct {
		Identity     string `json:"identity"`
		ScopeType    string `json:"scope_type"`
		Organization string `json:"organization"`
		Namespace    string `json:"namespace"`
		Repository   string `json:"repository"`
		Role         string `json:"role"`
		Effect       string `json:"effect"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if !authz.ValidRole(body.Role) {
		writeError(w, http.StatusBadRequest, "invalid_body", "unknown role "+body.Role)
		return
	}

	target, err := a.opts.Identities.ByName(r.Context(), body.Identity)
	if err != nil {
		notFound(w, "identity", body.Identity)
		return
	}

	params := identity.GrantParams{
		IdentityID: &target.ID,
		ScopeType:  body.ScopeType,
		Role:       authz.Role(body.Role),
		Effect:     body.Effect,
		CreatedBy:  &actor.ID,
	}
	switch body.ScopeType {
	case "instance":
	case "organization":
		var orgID int64
		if err := a.opts.Pool.QueryRow(r.Context(),
			`SELECT id FROM organizations WHERE slug = $1`, body.Organization).Scan(&orgID); err != nil {
			notFound(w, "organization", body.Organization)
			return
		}
		params.OrganizationID = &orgID
	case "namespace":
		params.NamespacePrefix = body.Namespace
	case "repository":
		repo, err := a.opts.Catalog.RepositoryByName(r.Context(), body.Repository)
		if err != nil {
			notFound(w, "repository", body.Repository)
			return
		}
		params.RepositoryID = &repo.ID
	default:
		writeError(w, http.StatusBadRequest, "invalid_body",
			"scope_type must be one of instance, organization, namespace, repository")
		return
	}

	if err := a.opts.Identities.Grant(r.Context(), params); err != nil {
		a.internalError(w, r, "creating a grant", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"granted": body.Role, "to": body.Identity})
}

// --- garbage collection ---

func (a *API) handleRunGC(w http.ResponseWriter, r *http.Request, _ []string, actor *identity.Identity) {
	dryRun := r.URL.Query().Get("dry_run") == "true"
	a.opts.Logger.Info("garbage collection requested", "dry_run", dryRun, "actor", actor.Name)

	stats, err := a.opts.Collector.Run(r.Context(), dryRun)
	if err != nil {
		// The stats are returned alongside the error: a partially completed run
		// still reclaimed what it reclaimed, and hiding that makes the failure
		// harder to interpret.
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"code": "gc_failed", "message": err.Error()},
			"stats": stats,
		})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (a *API) handleGCStatus(w http.ResponseWriter, r *http.Request, _ []string, _ *identity.Identity) {
	var (
		job        string
		status     string
		startedAt  time.Time
		finishedAt *time.Time
		stats      []byte
		errMessage *string
	)
	err := a.opts.Pool.QueryRow(r.Context(), `
		SELECT job, status, started_at, finished_at, stats, error
		FROM job_runs WHERE job IN ('gc', 'gc-dry-run')
		ORDER BY started_at DESC LIMIT 1`).Scan(
		&job, &status, &startedAt, &finishedAt, &stats, &errMessage)

	response := map[string]any{}
	if err != nil {
		response["last_run"] = nil
		response["note"] = "garbage collection has not run on this instance yet"
	} else {
		response["last_run"] = map[string]any{
			"job":         job,
			"status":      status,
			"started_at":  startedAt,
			"finished_at": finishedAt,
			"error":       errMessage,
			"stats":       jsonRaw(stats),
		}
	}

	// The quarantine backlog is what an operator most wants alongside this: it
	// is storage that is no longer served but not yet reclaimed.
	var quarantinedBlobs int
	var quarantinedBytes int64
	_ = a.opts.Pool.QueryRow(r.Context(), `
		SELECT count(*), coalesce(sum(size_bytes), 0) FROM blobs WHERE state = 'quarantined'`).
		Scan(&quarantinedBlobs, &quarantinedBytes)
	response["quarantined_blobs"] = quarantinedBlobs
	response["quarantined_bytes"] = quarantinedBytes

	stuck, _ := a.opts.Collector.StuckDeleting(r.Context(), 24*time.Hour)
	response["stuck_deleting"] = stuck
	if stuck > 0 {
		response["alert"] = "blobs have been stuck in the deleting state for over 24 hours; " +
			"storage deletion is failing — check the daemon log"
	}

	writeJSON(w, http.StatusOK, response)
}

func (a *API) handleReconcile(w http.ResponseWriter, r *http.Request, _ []string, actor *identity.Identity) {
	a.opts.Logger.Info("reconcile requested", "actor", actor.Name)
	report, err := a.opts.Collector.Reconcile(r.Context())
	if err != nil {
		a.internalError(w, r, "reconciling storage against the catalog", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// jsonRaw passes stored JSON through without re-encoding it as a string.
func jsonRaw(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return rawMessage(b)
}

type rawMessage []byte

func (r rawMessage) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

// isUniqueViolation reports a duplicate-key failure, matching the same
// detection used in the catalog and identity stores.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
