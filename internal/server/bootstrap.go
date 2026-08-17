package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mantle-sh/mantle/internal/auth/authz"
	"github.com/mantle-sh/mantle/internal/auth/identity"
)

// BootstrapResult reports what the first-run setup created.
type BootstrapResult struct {
	AdminUsername string
	AdminPassword string
	Organization  string
	// AlreadyBootstrapped reports that an admin already existed and nothing was
	// created. Re-running install must not mint a second admin account or reset
	// the first one's password.
	AlreadyBootstrapped bool
}

// Bootstrap creates the default organization and the first instance
// administrator.
//
// It is idempotent by design: `mantle install` may be re-run after a partial
// failure, and the one thing it must never do is silently replace the
// credentials of a working instance.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, username, password, orgSlug string) (*BootstrapResult, error) {
	if username == "" {
		username = "admin"
	}
	if orgSlug == "" {
		orgSlug = DefaultOrganization
	}

	identities := identity.New(pool)

	var adminCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM identities WHERE kind = 'user' AND instance_admin`).Scan(&adminCount); err != nil {
		return nil, fmt.Errorf("checking for an existing administrator: %w", err)
	}
	if adminCount > 0 {
		if err := ensureOrganization(ctx, pool, orgSlug); err != nil {
			return nil, err
		}
		return &BootstrapResult{Organization: orgSlug, AlreadyBootstrapped: true}, nil
	}

	generated := false
	if password == "" {
		var err error
		password, err = generatePassword()
		if err != nil {
			return nil, err
		}
		generated = true
	}

	if err := ensureOrganization(ctx, pool, orgSlug); err != nil {
		return nil, err
	}

	admin, err := identities.CreateUser(ctx, identity.CreateUserParams{
		Name:          username,
		DisplayName:   "Instance administrator",
		Password:      password,
		InstanceAdmin: true,
	})
	if err != nil {
		if errors.Is(err, identity.ErrAlreadyExists) {
			return nil, fmt.Errorf(
				"a user named %q already exists but is not an instance administrator; "+
					"choose a different --admin-username, or grant the existing user admin rights", username)
		}
		return nil, fmt.Errorf("creating the administrator account: %w", err)
	}

	// An instance-wide owner grant in addition to the instance_admin flag. The
	// flag short-circuits authorization, but the grant makes the administrator
	// visible in permission listings rather than appearing to hold nothing.
	if err := identities.Grant(ctx, identity.GrantParams{
		IdentityID: &admin.ID,
		ScopeType:  "instance",
		Role:       authz.RoleOwner,
		Effect:     "allow",
	}); err != nil {
		return nil, fmt.Errorf("granting instance ownership to the administrator: %w", err)
	}

	result := &BootstrapResult{
		AdminUsername: username,
		Organization:  orgSlug,
	}
	if generated {
		result.AdminPassword = password
	}
	return result, nil
}

// ensureOrganization creates an organization if it is absent.
func ensureOrganization(ctx context.Context, pool *pgxpool.Pool, slug string) error {
	// slug is citext and display_name is text, so the same placeholder cannot
	// serve both — Postgres cannot deduce one type for it. Bind twice.
	_, err := pool.Exec(ctx, `
		INSERT INTO organizations (slug, display_name)
		VALUES ($1, $2) ON CONFLICT (slug) DO NOTHING`, slug, slug)
	if err != nil {
		return fmt.Errorf("creating organization %q: %w", slug, err)
	}
	return nil
}

// CreateOrganization creates an organization, reporting a conflict.
func CreateOrganization(ctx context.Context, pool *pgxpool.Pool, slug, displayName string) error {
	if displayName == "" {
		displayName = slug
	}
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO organizations (slug, display_name) VALUES ($1, $2) RETURNING id`,
		slug, displayName).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("organization %q already exists", slug)
		}
		return err
	}
	return nil
}

// generatePassword produces a random initial administrator password.
//
// It is shown once, at install, and is not recoverable afterwards. 24 bytes of
// entropy is well beyond anything a password policy would ask for, and printing
// it is safer than asking an operator to invent one at the shell where it lands
// in their history.
func generatePassword() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating an administrator password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
