package catalog

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mantle-sh/mantle/internal/oci"
)

const manifestColumns = `id, repository_id, digest, media_type,
	coalesce(artifact_type, ''), coalesce(subject_digest, ''), size_bytes,
	payload, coalesce(config_digest, ''), pinned, state, created_at`

func scanManifest(row pgx.Row) (*Manifest, error) {
	var m Manifest
	err := row.Scan(&m.ID, &m.RepositoryID, &m.Digest, &m.MediaType, &m.ArtifactType,
		&m.SubjectDigest, &m.SizeBytes, &m.Payload, &m.ConfigDigest, &m.Pinned,
		&m.State, &m.CreatedAt)
	if err != nil {
		return nil, noRows(err)
	}
	return &m, nil
}

// ManifestByDigest resolves a manifest within a repository by content address.
func (s *Store) ManifestByDigest(ctx context.Context, repoID int64, digest string) (*Manifest, error) {
	return scanManifest(s.pool.QueryRow(ctx,
		`SELECT `+manifestColumns+` FROM manifests
		 WHERE repository_id = $1 AND digest = $2 AND state = 'available'`,
		repoID, digest))
}

// ManifestByTag resolves a manifest within a repository by tag.
//
// This is the first query of every pull and therefore the most latency-
// sensitive statement in the system. It is a single indexed join returning the
// payload bytes directly from the row, with no storage round-trip — which is
// the whole reason manifests live in Postgres (§11.3).
func (s *Store) ManifestByTag(ctx context.Context, repoID int64, tag string) (*Manifest, error) {
	return scanManifest(s.pool.QueryRow(ctx, `
		SELECT m.id, m.repository_id, m.digest, m.media_type,
		       coalesce(m.artifact_type, ''), coalesce(m.subject_digest, ''), m.size_bytes,
		       m.payload, coalesce(m.config_digest, ''), m.pinned, m.state, m.created_at
		FROM tags t
		JOIN manifests m ON m.id = t.manifest_id
		WHERE t.repository_id = $1 AND t.name = $2 AND m.state = 'available'`,
		repoID, tag))
}

// ManifestByReference resolves either form of manifest reference.
func (s *Store) ManifestByReference(ctx context.Context, repoID int64, reference string) (*Manifest, error) {
	if oci.IsDigestReference(reference) {
		return s.ManifestByDigest(ctx, repoID, reference)
	}
	return s.ManifestByTag(ctx, repoID, reference)
}

// PutManifestParams carries everything one manifest write needs.
type PutManifestParams struct {
	RepositoryID int64
	Digest       string
	// Tag is empty when the manifest was pushed by digest.
	Tag       string
	MediaType string
	Payload   []byte
	Parsed    *oci.Manifest
	ActorID   *int64
	// ImmutableTags and ProtectedPatterns come from repository policy (§15.2).
	ImmutableTags bool
}

// PutManifestResult reports what a manifest write did.
type PutManifestResult struct {
	Manifest *Manifest
	// TagMoved reports that an existing tag was repointed at different content.
	TagMoved bool
	// PreviousDigest is what the tag pointed at before, if it moved.
	PreviousDigest string
}

// PutManifest writes a manifest, its edges, and optionally its tag, atomically.
//
// The referential integrity check (REQ-OCI-05) runs inside this transaction
// rather than before it, and that placement is the point. Checking first and
// inserting afterwards leaves a window in which garbage collection could
// quarantine a blob between the two, producing a manifest that pushed
// successfully and pulls with a fatal error. Holding the check and the insert in
// one transaction, against edges declared ON DELETE RESTRICT, closes it: a
// concurrent sweep either runs before the check and is seen, or blocks on the
// row lock and then fails its own unlink (§12.1, mechanism 2).
func (s *Store) PutManifest(ctx context.Context, p PutManifestParams) (*PutManifestResult, error) {
	result := &PutManifestResult{}

	err := s.tx(ctx, func(tx pgx.Tx) error {
		// --- REQ-OCI-05: every referenced blob must already be present here ---
		blobRefs := p.Parsed.BlobReferences()
		blobIDs := make([]int64, 0, len(blobRefs))
		if len(blobRefs) > 0 {
			wanted := make([]string, 0, len(blobRefs))
			for _, d := range blobRefs {
				wanted = append(wanted, d.Digest)
			}

			// FOR SHARE on blobs holds the rows against a concurrent sweep for
			// the remainder of the transaction.
			rows, err := tx.Query(ctx, `
				SELECT b.digest, b.id
				FROM blobs b
				JOIN repository_blobs rb ON rb.blob_id = b.id
				WHERE rb.repository_id = $1 AND b.digest = ANY($2) AND b.state = 'available'
				FOR SHARE OF b`, p.RepositoryID, wanted)
			if err != nil {
				return err
			}
			present := map[string]int64{}
			for rows.Next() {
				var digest string
				var id int64
				if err := rows.Scan(&digest, &id); err != nil {
					rows.Close()
					return err
				}
				present[digest] = id
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}

			var missing []string
			for _, digest := range wanted {
				id, ok := present[digest]
				if !ok {
					missing = append(missing, digest)
					continue
				}
				blobIDs = append(blobIDs, id)
			}
			if len(missing) > 0 {
				return &ErrMissingBlobs{Digests: missing}
			}
		}

		// --- child manifests of an index must exist too ---
		childRefs := p.Parsed.ChildReferences()
		childIDs := make([]int64, 0, len(childRefs))
		if len(childRefs) > 0 {
			wanted := make([]string, 0, len(childRefs))
			for _, d := range childRefs {
				wanted = append(wanted, d.Digest)
			}
			rows, err := tx.Query(ctx, `
				SELECT digest, id FROM manifests
				WHERE repository_id = $1 AND digest = ANY($2) AND state = 'available'
				FOR SHARE`, p.RepositoryID, wanted)
			if err != nil {
				return err
			}
			present := map[string]int64{}
			for rows.Next() {
				var digest string
				var id int64
				if err := rows.Scan(&digest, &id); err != nil {
					rows.Close()
					return err
				}
				present[digest] = id
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}

			var missing []string
			for _, digest := range wanted {
				id, ok := present[digest]
				if !ok {
					missing = append(missing, digest)
					continue
				}
				childIDs = append(childIDs, id)
			}
			if len(missing) > 0 {
				return &ErrMissingBlobs{Digests: missing}
			}
		}

		// --- the manifest row ---
		annotations, err := json.Marshal(p.Parsed.Annotations)
		if err != nil {
			annotations = []byte("{}")
		}
		configDigest := ""
		if p.Parsed.Config != nil {
			configDigest = p.Parsed.Config.Digest
		}

		// A re-push of identical content is idempotent, which matters because
		// clients retry. ON CONFLICT touches nothing but the row's presence:
		// the payload is by definition identical, since the digest matched.
		row := tx.QueryRow(ctx, `
			INSERT INTO manifests (repository_id, digest, media_type, artifact_type,
			                       subject_digest, size_bytes, payload, config_digest,
			                       annotations, pushed_by, state)
			VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6, $7, nullif($8, ''), $9, $10, 'available')
			ON CONFLICT (repository_id, digest) DO UPDATE
			  SET state = 'available', quarantined_at = NULL
			RETURNING `+manifestColumns,
			p.RepositoryID, p.Digest, p.MediaType, p.Parsed.EffectiveArtifactType(),
			p.Parsed.SubjectDigest(), len(p.Payload), p.Payload, configDigest,
			annotations, p.ActorID)
		manifest, err := scanManifest(row)
		if err != nil {
			return err
		}
		result.Manifest = manifest

		// --- edges ---
		for _, blobID := range blobIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO manifest_blobs (manifest_id, blob_id)
				 VALUES ($1, $2) ON CONFLICT DO NOTHING`, manifest.ID, blobID); err != nil {
				return err
			}
		}
		for _, childID := range childIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO manifest_children (parent_id, child_id)
				 VALUES ($1, $2) ON CONFLICT DO NOTHING`, manifest.ID, childID); err != nil {
				return err
			}
		}

		// --- the tag, if this was a tagged push ---
		if p.Tag != "" {
			var previousDigest string
			var previousManifestID int64
			err := tx.QueryRow(ctx, `
				SELECT m.digest, m.id FROM tags t
				JOIN manifests m ON m.id = t.manifest_id
				WHERE t.repository_id = $1 AND t.name = $2
				FOR UPDATE OF t`, p.RepositoryID, p.Tag).Scan(&previousDigest, &previousManifestID)
			switch {
			case err == pgx.ErrNoRows:
				// New tag.
			case err != nil:
				return err
			case previousDigest == p.Digest:
				// Re-pushing the same content under the same tag. Not a move,
				// and specifically not an immutability violation — a retried
				// push must not fail on an immutable tag.
			case p.ImmutableTags:
				return fmt.Errorf("%w: %s already points at %s in a repository with immutable tags",
					ErrImmutable, p.Tag, previousDigest)
			default:
				result.TagMoved = true
				result.PreviousDigest = previousDigest
			}

			if _, err := tx.Exec(ctx, `
				INSERT INTO tags (repository_id, name, manifest_id, pushed_by)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (repository_id, name) DO UPDATE
				  SET manifest_id = EXCLUDED.manifest_id,
				      pushed_by = EXCLUDED.pushed_by,
				      updated_at = now()`,
				p.RepositoryID, p.Tag, manifest.ID, p.ActorID); err != nil {
				return err
			}

			action := "set"
			if result.TagMoved {
				action = "moved"
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO tag_history (repository_id, name, manifest_digest, action, actor_id)
				VALUES ($1, $2, $3, $4, $5)`,
				p.RepositoryID, p.Tag, p.Digest, action, p.ActorID); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteManifest removes a manifest from a repository (end-9).
//
// Tags pointing at it go first, since tags.manifest_id is RESTRICT. The blobs
// it referenced are left alone: unlinking them here would duplicate garbage
// collection's job and would do it without the grace window or the quarantine
// that make deletion recoverable (§12.3).
func (s *Store) DeleteManifest(ctx context.Context, repoID int64, digest string, actorID *int64) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var manifestID int64
		err := tx.QueryRow(ctx,
			`SELECT id FROM manifests WHERE repository_id = $1 AND digest = $2 FOR UPDATE`,
			repoID, digest).Scan(&manifestID)
		if err != nil {
			return noRows(err)
		}

		// Refuse if a deployment references it. This is the ledger guarantee
		// (§13.4) and it is enforced here as well as by the foreign key, so the
		// caller receives an explanation rather than a constraint violation.
		var deployments int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM deployments WHERE manifest_id = $1 AND status IN ('active','started')`,
			manifestID).Scan(&deployments); err != nil {
			return err
		}
		if deployments > 0 {
			return fmt.Errorf(
				"%w: %d active deployment(s) reference this image; "+
					"it cannot be deleted while it is running in production",
				ErrStillReferenced, deployments)
		}

		// An index that still has a parent must not be removed out from under it.
		var parents int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM manifest_children WHERE child_id = $1`, manifestID).Scan(&parents); err != nil {
			return err
		}
		if parents > 0 {
			return fmt.Errorf("%w: this manifest is a child of %d image index(es); delete those first",
				ErrStillReferenced, parents)
		}

		rows, err := tx.Query(ctx,
			`DELETE FROM tags WHERE manifest_id = $1 RETURNING name`, manifestID)
		if err != nil {
			return err
		}
		var removedTags []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			removedTags = append(removedTags, name)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, name := range removedTags {
			if _, err := tx.Exec(ctx, `
				INSERT INTO tag_history (repository_id, name, manifest_digest, action, actor_id)
				VALUES ($1, $2, $3, 'deleted', $4)`, repoID, name, digest, actorID); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `DELETE FROM manifest_blobs WHERE manifest_id = $1`, manifestID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM manifest_children WHERE parent_id = $1`, manifestID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM manifests WHERE id = $1`, manifestID)
		return err
	})
}

// Referrer is one entry in a referrers response.
type Referrer struct {
	Digest       string
	MediaType    string
	ArtifactType string
	Size         int
	Annotations  map[string]string
}

// Referrers lists manifests whose subject is the given digest (end-12a/12b).
//
// artifactType filters the result; the caller sets OCI-Filters-Applied when it
// is non-empty. An unknown subject is not an error — the specification requires
// an empty list with 200, because a client asking "are there signatures for
// this?" must be able to learn "no" without special-casing a 404.
func (s *Store) Referrers(ctx context.Context, repoID int64, subject string, artifactType string) ([]Referrer, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT digest, media_type, coalesce(artifact_type, ''), size_bytes, annotations
		FROM manifests
		WHERE repository_id = $1 AND subject_digest = $2 AND state = 'available'
		  AND ($3 = '' OR artifact_type = $3)
		ORDER BY created_at`,
		repoID, subject, artifactType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	referrers := []Referrer{}
	for rows.Next() {
		var r Referrer
		var annotations []byte
		if err := rows.Scan(&r.Digest, &r.MediaType, &r.ArtifactType, &r.Size, &annotations); err != nil {
			return nil, err
		}
		if len(annotations) > 0 {
			_ = json.Unmarshal(annotations, &r.Annotations)
		}
		referrers = append(referrers, r)
	}
	return referrers, rows.Err()
}
