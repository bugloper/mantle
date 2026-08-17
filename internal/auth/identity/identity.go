// Package identity manages the principals that authenticate to Mantle: users,
// personal access tokens, robot accounts, and deploy tokens (§9.2).
package identity

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mantle-sh/mantle/internal/auth/authz"
)

// Kind distinguishes the identity types.
type Kind string

const (
	KindUser        Kind = "user"
	KindPAT         Kind = "pat"
	KindRobot       Kind = "robot"
	KindDeployToken Kind = "deploy_token"
)

var (
	ErrNotFound      = errors.New("identity not found")
	ErrAlreadyExists = errors.New("identity already exists")
	// ErrAuthentication is returned for every authentication failure, whatever
	// the cause. Distinguishing "no such user" from "wrong password" tells an
	// attacker which usernames exist (SEC-08).
	ErrAuthentication = errors.New("authentication failed")
	ErrDisabled       = errors.New("identity is disabled")
	ErrExpired        = errors.New("credential has expired")
)

// Identity is a principal.
type Identity struct {
	ID             int64
	UUID           string
	Kind           Kind
	Name           string
	OrganizationID *int64
	OwnerID        *int64
	Email          string
	DisplayName    string
	InstanceAdmin  bool
	Disabled       bool
	ExpiresAt      *time.Time
	LastUsedAt     *time.Time
	CreatedAt      time.Time
}

// Subject is the identity's URN, used as the JWT `sub` claim.
func (i *Identity) Subject() string {
	return fmt.Sprintf("mantle:%s:%s", i.Kind, i.UUID)
}

// Usable reports whether the identity may authenticate right now.
func (i *Identity) Usable() error {
	if i.Disabled {
		return ErrDisabled
	}
	if i.ExpiresAt != nil && time.Now().After(*i.ExpiresAt) {
		return ErrExpired
	}
	return nil
}

// Store manages identities.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const identityColumns = `id, uuid::text, kind, name::text, organization_id, owner_id,
	coalesce(email::text, ''), coalesce(display_name, ''), instance_admin, disabled,
	expires_at, last_used_at, created_at`

func scanIdentity(row pgx.Row) (*Identity, error) {
	var i Identity
	err := row.Scan(&i.ID, &i.UUID, &i.Kind, &i.Name, &i.OrganizationID, &i.OwnerID,
		&i.Email, &i.DisplayName, &i.InstanceAdmin, &i.Disabled,
		&i.ExpiresAt, &i.LastUsedAt, &i.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &i, nil
}

// CreateUserParams describes a new human account.
type CreateUserParams struct {
	Name          string
	Email         string
	DisplayName   string
	Password      string
	InstanceAdmin bool
	CreatedBy     *int64
}

// CreateUser creates a human account.
func (s *Store) CreateUser(ctx context.Context, p CreateUserParams) (*Identity, error) {
	if p.Password == "" {
		return nil, fmt.Errorf("a password is required")
	}
	hash, err := HashSecret(p.Password)
	if err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO identities (kind, name, email, display_name, secret_hash, instance_admin, created_by)
		VALUES ('user', $1, nullif($2, ''), nullif($3, ''), $4, $5, $6)
		RETURNING `+identityColumns,
		p.Name, p.Email, p.DisplayName, hash, p.InstanceAdmin, p.CreatedBy)
	identity, err := scanIdentity(row)
	if isUnique(err) {
		return nil, ErrAlreadyExists
	}
	return identity, err
}

// CreateMachineParams describes a new PAT, robot, or deploy token.
type CreateMachineParams struct {
	Kind           Kind
	Name           string
	OrganizationID int64
	// OwnerID links a PAT to the human who created it, so audit records name
	// the person behind the credential rather than only the token.
	OwnerID   *int64
	ExpiresAt *time.Time
	CreatedBy *int64
}

// CreateMachine creates a machine identity and returns it alongside its
// plaintext secret, which is shown exactly once and never recoverable.
func (s *Store) CreateMachine(ctx context.Context, p CreateMachineParams) (*Identity, string, error) {
	if p.Kind == KindUser {
		return nil, "", fmt.Errorf("use CreateUser for human accounts")
	}
	credential, err := GenerateCredential(p.Kind)
	if err != nil {
		return nil, "", err
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO identities (kind, name, organization_id, owner_id, secret_hash,
		                        secret_selector, expires_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+identityColumns,
		p.Kind, p.Name, p.OrganizationID, p.OwnerID, credential.Hash,
		credential.Selector, p.ExpiresAt, p.CreatedBy)
	identity, err := scanIdentity(row)
	if isUnique(err) {
		return nil, "", ErrAlreadyExists
	}
	if err != nil {
		return nil, "", err
	}
	return identity, credential.Plaintext, nil
}

// Authenticate resolves a username and secret to an identity.
//
// Both credential shapes arrive through the same HTTP Basic header, so the
// secret's own prefix decides how it is checked: a machine credential is looked
// up by selector and the supplied username is ignored, while anything else is
// treated as a username and password.
//
// Every failure returns ErrAuthentication with no detail. The caller logs the
// reason; the client learns only that it failed (SEC-08).
func (s *Store) Authenticate(ctx context.Context, username, secret string) (*Identity, error) {
	if parsed, ok := ParseCredential(secret); ok {
		return s.authenticateMachine(ctx, parsed)
	}
	return s.authenticateUser(ctx, username, secret)
}

func (s *Store) authenticateMachine(ctx context.Context, parsed *ParsedCredential) (*Identity, error) {
	var hash string
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id, coalesce(secret_hash, '') FROM identities WHERE secret_selector = $1`,
		parsed.Selector).Scan(&id, &hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Spend the hashing cost anyway so that an unknown selector and a
			// wrong secret take indistinguishable time.
			VerifySecret(parsed.Verifier, decoyHash)
			return nil, ErrAuthentication
		}
		return nil, err
	}
	if !VerifySecret(parsed.Verifier, hash) {
		return nil, ErrAuthentication
	}

	identity, err := s.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := identity.Usable(); err != nil {
		return nil, err
	}
	s.touch(ctx, identity.ID)
	return identity, nil
}

func (s *Store) authenticateUser(ctx context.Context, username, password string) (*Identity, error) {
	if username == "" || password == "" {
		return nil, ErrAuthentication
	}
	var hash string
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id, coalesce(secret_hash, '') FROM identities WHERE kind = 'user' AND name = $1`,
		username).Scan(&id, &hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			VerifySecret(password, decoyHash)
			return nil, ErrAuthentication
		}
		return nil, err
	}
	if !VerifySecret(password, hash) {
		return nil, ErrAuthentication
	}

	identity, err := s.ByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := identity.Usable(); err != nil {
		return nil, err
	}
	s.touch(ctx, identity.ID)
	return identity, nil
}

// decoyHash is an Argon2id hash of a random value generated at startup.
// Verifying a presented secret against it makes the unknown-principal path cost
// the same as the wrong-secret path, closing a user-enumeration timing channel
// (SEC-08).
//
// It is generated rather than hardcoded so it is guaranteed to parse. A literal
// that failed to decode would make VerifySecret return early without doing any
// work, which is precisely the timing difference this exists to remove.
var decoyHash = func() string {
	filler := make([]byte, 32)
	if _, err := rand.Read(filler); err != nil {
		// Falling back to a fixed string costs the timing property, not
		// correctness; a failing CSPRNG will break authentication elsewhere first.
		filler = []byte("mantle-decoy-fallback")
	}
	hash, err := HashSecret(string(filler))
	if err != nil {
		return ""
	}
	return hash
}()

// touch records last use, best effort. A failure here must never fail an
// otherwise valid authentication.
func (s *Store) touch(ctx context.Context, id int64) {
	_, _ = s.pool.Exec(context.WithoutCancel(ctx),
		`UPDATE identities SET last_used_at = now() WHERE id = $1`, id)
}

// ByID loads an identity.
func (s *Store) ByID(ctx context.Context, id int64) (*Identity, error) {
	return scanIdentity(s.pool.QueryRow(ctx,
		`SELECT `+identityColumns+` FROM identities WHERE id = $1`, id))
}

// ByUUID loads an identity by its public identifier.
func (s *Store) ByUUID(ctx context.Context, uuid string) (*Identity, error) {
	return scanIdentity(s.pool.QueryRow(ctx,
		`SELECT `+identityColumns+` FROM identities WHERE uuid = $1::uuid`, uuid))
}

// ByName loads a user by name.
func (s *Store) ByName(ctx context.Context, name string) (*Identity, error) {
	return scanIdentity(s.pool.QueryRow(ctx,
		`SELECT `+identityColumns+` FROM identities WHERE kind = 'user' AND name = $1`, name))
}

// List returns identities, optionally filtered by kind and organization.
func (s *Store) List(ctx context.Context, kind Kind, orgID *int64) ([]*Identity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+identityColumns+` FROM identities
		WHERE ($1 = '' OR kind = $1)
		  AND ($2::bigint IS NULL OR organization_id = $2::bigint)
		ORDER BY kind, name`, string(kind), orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var identities []*Identity
	for rows.Next() {
		identity, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

// SetDisabled disables or re-enables an identity.
//
// Disabling is preferred to deletion for a compromised credential: the audit
// trail references the identity, and deleting it would take the record of what
// the credential did with it.
func (s *Store) SetDisabled(ctx context.Context, id int64, disabled bool, reason string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE identities SET disabled = $2, disabled_reason = nullif($3, ''), updated_at = now()
		WHERE id = $1`, id, disabled, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes an identity outright.
func (s *Store) Delete(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM identities WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPassword replaces a user's password.
func (s *Store) SetPassword(ctx context.Context, id int64, password string) error {
	hash, err := HashSecret(password)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE identities SET secret_hash = $2, updated_at = now() WHERE id = $1 AND kind = 'user'`,
		id, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- grants ---

// GrantParams describes an authorization grant to create.
type GrantParams struct {
	IdentityID      *int64
	TeamID          *int64
	ScopeType       string
	OrganizationID  *int64
	NamespacePrefix string
	RepositoryID    *int64
	Role            authz.Role
	Effect          string
	CreatedBy       *int64
}

// Grant creates an authorization grant.
func (s *Store) Grant(ctx context.Context, p GrantParams) error {
	if !authz.ValidRole(string(p.Role)) {
		return fmt.Errorf("unknown role %q", p.Role)
	}
	principalType := "identity"
	if p.TeamID != nil {
		principalType = "team"
	}
	effect := p.Effect
	if effect == "" {
		effect = "allow"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO grants (principal_type, identity_id, team_id, scope_type,
		                    organization_id, namespace_prefix, repository_id,
		                    role, effect, created_by)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7, $8, $9, $10)`,
		principalType, p.IdentityID, p.TeamID, p.ScopeType,
		p.OrganizationID, p.NamespacePrefix, p.RepositoryID,
		string(p.Role), effect, p.CreatedBy)
	return err
}

// PermissionsForRepository resolves what an identity may do to one repository.
//
// This is the query on the authorization path of every push and of every pull
// of a private image. It collects every grant that covers the repository — by
// instance, organization, namespace prefix, or directly — for the identity and
// for every team it belongs to, and resolves them in one pass.
func (s *Store) PermissionsForRepository(ctx context.Context, identityID int64, repoID int64) (authz.Permissions, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.role, g.effect, g.scope_type
		FROM grants g
		JOIN repositories r ON r.id = $2
		WHERE (g.identity_id = $1
		       OR g.team_id IN (SELECT team_id FROM team_members WHERE identity_id = $1))
		  AND (g.scope_type = 'instance'
		       OR (g.scope_type = 'organization' AND g.organization_id = r.organization_id)
		       OR (g.scope_type = 'namespace' AND r.name LIKE g.namespace_prefix || '%')
		       OR (g.scope_type = 'repository' AND g.repository_id = r.id))`,
		identityID, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var grants []authz.Grant
	for rows.Next() {
		var g authz.Grant
		if err := rows.Scan(&g.Role, &g.Effect, &g.ScopeType); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return authz.Resolve(grants), nil
}

// PermissionsForName resolves permissions for a repository that may not exist
// yet, which is the case on the first push to a new name.
//
// Grants are matched against the name rather than a row, so a namespace grant
// authorises creating repositories under it. Without this, auto-creation on
// push could not be authorised at all and every new repository would need an
// out-of-band setup step.
func (s *Store) PermissionsForName(ctx context.Context, identityID int64, name string) (authz.Permissions, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.role, g.effect, g.scope_type
		FROM grants g
		LEFT JOIN organizations o ON o.id = g.organization_id
		LEFT JOIN repositories r ON r.id = g.repository_id
		WHERE (g.identity_id = $1
		       OR g.team_id IN (SELECT team_id FROM team_members WHERE identity_id = $1))
		  AND (g.scope_type = 'instance'
		       OR (g.scope_type = 'organization' AND $2 LIKE o.slug || '/%')
		       OR (g.scope_type = 'namespace' AND $2 LIKE g.namespace_prefix || '%')
		       OR (g.scope_type = 'repository' AND r.name = $2))`,
		identityID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var grants []authz.Grant
	for rows.Next() {
		var g authz.Grant
		if err := rows.Scan(&g.Role, &g.Effect, &g.ScopeType); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return authz.Resolve(grants), nil
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
