package catalog

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/jackc/pgx/v5"
)

// TagPage is one page of a tag listing.
type TagPage struct {
	Tags    []string
	HasMore bool
}

// ListTags returns tag names in lexical order (REQ-OCI-08).
//
// Pagination is keyset, not offset: `WHERE name > last ORDER BY name LIMIT n`
// is a range scan over tags_lexical_idx and stays fast regardless of how deep
// the client has paged. OFFSET would degrade linearly, which on a repository
// with thousands of tags turns the last page into a table scan.
func (s *Store) ListTags(ctx context.Context, repoID int64, n int, last string) (*TagPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.name
		FROM tags t
		JOIN manifests m ON m.id = t.manifest_id
		WHERE t.repository_id = $1 AND t.name > $2 AND m.state = 'available'
		ORDER BY t.name
		LIMIT $3`, repoID, last, n+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	page := &TagPage{Tags: []string{}}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		page.Tags = append(page.Tags, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Tags) > n {
		page.Tags = page.Tags[:n]
		page.HasMore = true
	}
	return page, nil
}

// TagByName resolves a single tag.
func (s *Store) TagByName(ctx context.Context, repoID int64, name string) (*Tag, error) {
	var t Tag
	err := s.pool.QueryRow(ctx, `
		SELECT t.id, t.repository_id, t.name, t.manifest_id, m.digest, t.protected, t.updated_at
		FROM tags t JOIN manifests m ON m.id = t.manifest_id
		WHERE t.repository_id = $1 AND t.name = $2`, repoID, name).Scan(
		&t.ID, &t.RepositoryID, &t.Name, &t.ManifestID, &t.ManifestDigest, &t.Protected, &t.UpdatedAt)
	if err != nil {
		return nil, noRows(err)
	}
	return &t, nil
}

// DeleteTag removes a tag without touching the manifest it pointed at.
func (s *Store) DeleteTag(ctx context.Context, repoID int64, name string, actorID *int64) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var digest string
		var protected bool
		err := tx.QueryRow(ctx, `
			SELECT m.digest, t.protected FROM tags t
			JOIN manifests m ON m.id = t.manifest_id
			WHERE t.repository_id = $1 AND t.name = $2 FOR UPDATE OF t`,
			repoID, name).Scan(&digest, &protected)
		if err != nil {
			return noRows(err)
		}

		var immutable bool
		if err := tx.QueryRow(ctx,
			`SELECT immutable_tags FROM repositories WHERE id = $1`, repoID).Scan(&immutable); err != nil {
			return err
		}
		if immutable {
			return fmt.Errorf("%w: %s cannot be deleted in a repository with immutable tags", ErrImmutable, name)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM tags WHERE repository_id = $1 AND name = $2`, repoID, name); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO tag_history (repository_id, name, manifest_digest, action, actor_id)
			VALUES ($1, $2, $3, 'deleted', $4)`, repoID, name, digest, actorID)
		return err
	})
}

// TagProtectionRule is a pattern-scoped policy over tag names (§15.2).
type TagProtectionRule struct {
	ID        int64
	Pattern   string
	Immutable bool
	MinRole   string
}

// TagProtection returns the rules for a repository.
func (s *Store) TagProtection(ctx context.Context, repoID int64) ([]TagProtectionRule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, pattern, immutable, min_role FROM tag_protection_rules
		 WHERE repository_id = $1 ORDER BY pattern`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []TagProtectionRule
	for rows.Next() {
		var r TagProtectionRule
		if err := rows.Scan(&r.ID, &r.Pattern, &r.Immutable, &r.MinRole); err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// AddTagProtection creates or replaces a rule.
func (s *Store) AddTagProtection(ctx context.Context, repoID int64, pattern string, immutable bool, minRole string) error {
	if _, err := path.Match(pattern, "probe"); err != nil {
		return fmt.Errorf("tag pattern %q is not a valid glob: %w", pattern, err)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tag_protection_rules (repository_id, pattern, immutable, min_role)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (repository_id, pattern) DO UPDATE
		  SET immutable = EXCLUDED.immutable, min_role = EXCLUDED.min_role`,
		repoID, pattern, immutable, minRole)
	return err
}

// MatchProtection returns the first rule matching a tag name, or nil.
//
// Glob matching uses path.Match, so `v*` behaves as operators expect and
// `feature/*` matches one path segment. Tags rarely contain slashes, so the
// distinction almost never bites; where it does, the rule list is short enough
// to read.
func MatchProtection(rules []TagProtectionRule, tag string) *TagProtectionRule {
	for i := range rules {
		if ok, err := path.Match(rules[i].Pattern, tag); err == nil && ok {
			return &rules[i]
		}
	}
	return nil
}

// TagHistoryEntry is one recorded tag transition.
type TagHistoryEntry struct {
	Name           string
	ManifestDigest string
	Action         string
	ActorName      string
	OccurredAt     time.Time
}

// TagHistory returns recent transitions for a repository, most recent first.
func (s *Store) TagHistory(ctx context.Context, repoID int64, tagName string, limit int) ([]TagHistoryEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT h.name, h.manifest_digest, h.action, coalesce(i.name::text, ''), h.occurred_at
		FROM tag_history h
		LEFT JOIN identities i ON i.id = h.actor_id
		WHERE h.repository_id = $1 AND ($2 = '' OR h.name = $2)
		ORDER BY h.occurred_at DESC
		LIMIT $3`, repoID, tagName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []TagHistoryEntry
	for rows.Next() {
		var e TagHistoryEntry
		if err := rows.Scan(&e.Name, &e.ManifestDigest, &e.Action, &e.ActorName, &e.OccurredAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// TagsForManifest lists the tags currently pointing at a manifest.
func (s *Store) TagsForManifest(ctx context.Context, manifestID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT name FROM tags WHERE manifest_id = $1 ORDER BY name`, manifestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tags := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tags = append(tags, name)
	}
	return tags, rows.Err()
}
