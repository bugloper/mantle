package admin

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/mantle-sh/mantle/internal/auth/identity"
	"github.com/mantle-sh/mantle/internal/catalog"
	"github.com/mantle-sh/mantle/internal/ledger"
	"github.com/mantle-sh/mantle/internal/oci"
)

// recordDeploymentRequest is the Tier 1 deploy report (§13.2).
//
// Every field except the repository and a digest-or-tag is optional, and that
// permissiveness is the design. This endpoint is the one thing a user has to
// wire into their own deploy process, so it has to accept whatever they can
// easily produce — a report carrying only a repository and a digest still
// upgrades the record from inferred to reported, which is the whole point.
type recordDeploymentRequest struct {
	Repository  string         `json:"repository"`
	Digest      string         `json:"digest"`
	Tag         string         `json:"tag"`
	Environment string         `json:"environment"`
	Status      string         `json:"status"`
	CommitSHA   string         `json:"commit_sha"`
	Performer   string         `json:"performer"`
	DeployTool  string         `json:"deploy_tool"`
	ToolVersion string         `json:"deploy_tool_version"`
	ExternalID  string         `json:"external_id"`
	Hosts       []string       `json:"hosts"`
	Host        string         `json:"host"`
	Metadata    map[string]any `json:"metadata"`
}

// handleRecordDeployment implements POST /api/v1/deployments.
//
// It is deliberately quick and forgiving. The documented invocation ends in
// `|| true`, and a failure to record a deployment must never be able to fail
// the deployment itself (REQ-LEDGER-02).
func (a *API) handleRecordDeployment(w http.ResponseWriter, r *http.Request, _ []string, actor *identity.Identity) {
	var body recordDeploymentRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error(),
			"the only required fields are repository and one of digest or tag")
		return
	}
	if body.Repository == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "repository is required")
		return
	}
	if body.Digest == "" && body.Tag == "" {
		writeError(w, http.StatusBadRequest, "invalid_body",
			"one of digest or tag is required, so the report can be tied to an image")
		return
	}

	repo, err := a.opts.Catalog.RepositoryByName(r.Context(), body.Repository)
	if errors.Is(err, catalog.ErrNotFound) {
		notFound(w, "repository", body.Repository)
		return
	}
	if err != nil {
		a.internalError(w, r, "resolving the repository", err)
		return
	}

	// Reporting a deployment is a statement about a repository, so it requires
	// the ability to read that repository. A pull-only deploy token — which is
	// what a server holds — therefore suffices, and no separate credential has
	// to be provisioned (§13.2).
	if !a.mayRead(r, actor, body.Repository) {
		notFound(w, "repository", body.Repository)
		return
	}

	reference := body.Digest
	if reference == "" {
		reference = body.Tag
	}
	manifest, err := a.opts.Catalog.ManifestByReference(r.Context(), repo.ID, reference)
	if errors.Is(err, catalog.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found",
			"no manifest matching "+reference+" in "+body.Repository,
			"push the image before reporting a deployment of it")
		return
	}
	if err != nil {
		a.internalError(w, r, "resolving the manifest", err)
		return
	}

	// Fill the commit from stored provenance when the caller did not supply one.
	// A deploy hook rarely knows the commit; the image usually does.
	commitSHA := body.CommitSHA
	if commitSHA == "" {
		if provenance, err := a.opts.Ledger.ProvenanceFor(r.Context(), manifest.ID); err == nil {
			commitSHA = provenance.CommitSHA
		}
	}

	hosts := body.Hosts
	if body.Host != "" {
		hosts = append(hosts, body.Host)
	}

	// Resolve a human-readable tag for the record. Deploying by digest is the
	// common and correct thing to do — it is what `docker inspect` yields — but
	// a ledger entry reading "(untagged)" is far less useful during an incident
	// than "v2.4.1", so the tag currently pointing at that manifest is adopted
	// when the caller did not name one.
	tag := body.Tag
	if tag == "" {
		if !oci.IsDigestReference(reference) {
			tag = reference
		} else if tags, err := a.opts.Catalog.TagsForManifest(r.Context(), manifest.ID); err == nil && len(tags) > 0 {
			tag = tags[0]
		}
	}

	performer := body.Performer
	if performer == "" && actor != nil {
		performer = actor.Name
	}

	deployment, err := a.opts.Ledger.RecordDeployment(r.Context(), repo.OrganizationID,
		ledger.RecordDeploymentParams{
			RepositoryID: repo.ID,
			ManifestID:   manifest.ID,
			Tag:          tag,
			Environment:  body.Environment,
			Status:       body.Status,
			Confidence:   ledger.ConfidenceReported,
			CommitSHA:    commitSHA,
			Performer:    performer,
			DeployTool:   body.DeployTool,
			ToolVersion:  body.ToolVersion,
			ExternalID:   body.ExternalID,
			Hostnames:    hosts,
			Metadata:     body.Metadata,
		})
	if err != nil {
		a.internalError(w, r, "recording a deployment", err)
		return
	}

	a.opts.Logger.Info("recorded deployment",
		"repository", body.Repository,
		"digest", manifest.Digest,
		"environment", deployment.Environment,
		"status", deployment.Status,
		"hosts", len(hosts),
		"performer", performer)

	writeJSON(w, http.StatusCreated, toDeploymentView(deployment, manifest.Digest))
}

type deploymentView struct {
	UUID        string     `json:"uuid"`
	Digest      string     `json:"digest"`
	Tag         string     `json:"tag,omitempty"`
	Environment string     `json:"environment"`
	Status      string     `json:"status"`
	Confidence  string     `json:"confidence"`
	CommitSHA   string     `json:"commit_sha,omitempty"`
	Performer   string     `json:"performer,omitempty"`
	DeployTool  string     `json:"deploy_tool,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Hosts       []hostView `json:"hosts"`
}

type hostView struct {
	Hostname string `json:"hostname,omitempty"`
	Address  string `json:"address,omitempty"`
	Status   string `json:"status"`
}

func toDeploymentView(d *ledger.Deployment, digest string) deploymentView {
	view := deploymentView{
		UUID: d.UUID, Digest: digest, Tag: d.Tag, Environment: d.Environment,
		Status: d.Status, Confidence: d.Confidence, CommitSHA: d.CommitSHA,
		Performer: d.Performer, DeployTool: d.DeployTool,
		StartedAt: d.StartedAt, CompletedAt: d.CompletedAt,
		Hosts: []hostView{},
	}
	for _, h := range d.Hosts {
		view.Hosts = append(view.Hosts, hostView{
			Hostname: h.Hostname, Address: h.Address, Status: h.Status,
		})
	}
	return view
}

func (a *API) handleListDeployments(w http.ResponseWriter, r *http.Request, _ []string, actor *identity.Identity) {
	repoName := r.URL.Query().Get("repository")
	if repoName == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"the repository query parameter is required")
		return
	}
	if !a.mayRead(r, actor, repoName) {
		notFound(w, "repository", repoName)
		return
	}
	repo, err := a.opts.Catalog.RepositoryByName(r.Context(), repoName)
	if mapStoreError(w, err, "repository", repoName) {
		return
	}
	if err != nil {
		a.internalError(w, r, "resolving the repository", err)
		return
	}

	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 500 {
			limit = parsed
		}
	}

	deployments, err := a.opts.Ledger.DeploymentHistory(r.Context(), repo.ID,
		r.URL.Query().Get("environment"), limit)
	if err != nil {
		a.internalError(w, r, "listing deployments", err)
		return
	}

	views := make([]deploymentView, 0, len(deployments))
	for _, d := range deployments {
		d.Hosts, _ = a.opts.Ledger.DeploymentHosts(r.Context(), d.ID)
		views = append(views, toDeploymentView(d, d.ManifestDigest))
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": views})
}

// ledgerView is the composed resource behind `mantle ledger status` (§13.5).
//
// It is deliberately one response rather than several endpoints. This is what a
// future UI page needs and what a monitoring script wants, and designing it as
// a single resource now avoids the N+1 API that gets built when a UI is bolted
// on later.
type ledgerView struct {
	Repository string `json:"repository"`
	SourceURL  string `json:"source_url,omitempty"`

	Running  *deploymentView  `json:"running"`
	Rollback []rollbackTarget `json:"rollback_candidates"`
	Tags     []tagView        `json:"tags"`
	Storage  storageView      `json:"storage"`

	// Environments lists every environment the ledger has observed, so a
	// caller learns that "staging" exists without having to guess the name.
	Environments []string `json:"environments"`
}

type rollbackTarget struct {
	Digest     string    `json:"digest"`
	Tag        string    `json:"tag,omitempty"`
	CommitSHA  string    `json:"commit_sha,omitempty"`
	DeployedAt time.Time `json:"deployed_at"`
	// Pinned reports that this image cannot be collected or expired while it
	// remains a rollback target (§13.4). It is the guarantee, surfaced.
	Pinned bool `json:"pinned"`
}

type tagView struct {
	Name      string    `json:"name"`
	Digest    string    `json:"digest"`
	UpdatedAt time.Time `json:"updated_at"`
}

type storageView struct {
	TotalBytes  int64 `json:"total_bytes"`
	Manifests   int   `json:"manifests"`
	Reclaimable int64 `json:"reclaimable_bytes"`
	Quarantined int64 `json:"quarantined_bytes"`
}

func (a *API) handleRepositoryLedger(w http.ResponseWriter, r *http.Request, m []string, actor *identity.Identity) {
	name := m[1]
	if !a.mayRead(r, actor, name) {
		notFound(w, "repository", name)
		return
	}
	repo, err := a.opts.Catalog.RepositoryByName(r.Context(), name)
	if mapStoreError(w, err, "repository", name) {
		return
	}
	if err != nil {
		a.internalError(w, r, "resolving the repository", err)
		return
	}

	environment := r.URL.Query().Get("environment")
	if environment == "" {
		environment = "production"
	}

	view := ledgerView{
		Repository: repo.Name,
		SourceURL:  repo.SourceURL,
		Rollback:   []rollbackTarget{},
		Tags:       []tagView{},
	}

	// --- what is running now ---
	running, err := a.opts.Ledger.ActiveDeployment(r.Context(), repo.ID, environment)
	if err == nil {
		v := toDeploymentView(running, running.ManifestDigest)
		view.Running = &v
	}

	// --- what could be rolled back to ---
	history, err := a.opts.Ledger.DeploymentHistory(r.Context(), repo.ID, environment,
		a.opts.RollbackDepth+2)
	if err == nil {
		pinnedIDs, _ := a.opts.Ledger.PinnedManifests(r.Context(), a.opts.RollbackDepth)
		pinned := map[int64]bool{}
		for _, id := range pinnedIDs {
			pinned[id] = true
		}
		for _, d := range history {
			if running != nil && d.ID == running.ID {
				continue
			}
			if len(view.Rollback) >= a.opts.RollbackDepth {
				break
			}
			view.Rollback = append(view.Rollback, rollbackTarget{
				Digest:     d.ManifestDigest,
				Tag:        d.Tag,
				CommitSHA:  d.CommitSHA,
				DeployedAt: d.StartedAt,
				Pinned:     pinned[d.ManifestID],
			})
		}
	}

	// --- tags ---
	tagRows, err := a.opts.Pool.Query(r.Context(), `
		SELECT t.name, m.digest, t.updated_at
		FROM tags t JOIN manifests m ON m.id = t.manifest_id
		WHERE t.repository_id = $1 ORDER BY t.updated_at DESC LIMIT 50`, repo.ID)
	if err == nil {
		for tagRows.Next() {
			var tv tagView
			if err := tagRows.Scan(&tv.Name, &tv.Digest, &tv.UpdatedAt); err == nil {
				view.Tags = append(view.Tags, tv)
			}
		}
		tagRows.Close()
	}

	// --- storage ---
	view.Storage.TotalBytes, _ = a.opts.Catalog.RepositoryUsage(r.Context(), repo.ID)
	_ = a.opts.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM manifests WHERE repository_id = $1 AND state = 'available'`,
		repo.ID).Scan(&view.Storage.Manifests)
	_ = a.opts.Pool.QueryRow(r.Context(), `
		SELECT coalesce(sum(b.size_bytes), 0)
		FROM blobs b JOIN repository_blobs rb ON rb.blob_id = b.id
		WHERE rb.repository_id = $1 AND b.state = 'quarantined'`,
		repo.ID).Scan(&view.Storage.Quarantined)

	// Reclaimable: blobs this repository references that no available manifest
	// in it needs. An estimate, and labelled as such by its name — the
	// authoritative answer comes from a GC dry run.
	_ = a.opts.Pool.QueryRow(r.Context(), `
		SELECT coalesce(sum(b.size_bytes), 0)
		FROM blobs b
		JOIN repository_blobs rb ON rb.blob_id = b.id
		WHERE rb.repository_id = $1
		  AND b.state = 'available'
		  AND NOT EXISTS (
		    SELECT 1 FROM manifest_blobs mb
		    JOIN manifests m ON m.id = mb.manifest_id
		    WHERE mb.blob_id = b.id AND m.repository_id = $1 AND m.state = 'available')`,
		repo.ID).Scan(&view.Storage.Reclaimable)

	// --- environments ---
	envRows, err := a.opts.Pool.Query(r.Context(),
		`SELECT DISTINCT environment FROM deployments WHERE repository_id = $1 ORDER BY environment`,
		repo.ID)
	if err == nil {
		for envRows.Next() {
			var env string
			if err := envRows.Scan(&env); err == nil {
				view.Environments = append(view.Environments, env)
			}
		}
		envRows.Close()
	}
	if view.Environments == nil {
		view.Environments = []string{}
	}

	writeJSON(w, http.StatusOK, view)
}
