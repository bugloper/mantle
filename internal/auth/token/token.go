// Package token implements the Docker Registry v2 token authentication flow
// (§9.1): issuing scoped RS256 JWTs and verifying them on every request.
//
// This is the protocol every OCI client already speaks. Nothing here is novel,
// and that is the point — a registry that invents its own authentication is a
// registry that does not work with `docker login`.
package token

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mantle-sh/mantle/internal/auth/authz"
)

// Access is one entry of the `access` claim, in the shape the Docker token
// specification defines.
type Access struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// Claims is a Mantle registry token.
type Claims struct {
	jwt.RegisteredClaims
	Access []Access `json:"access"`
	// Kind records the identity type, so the resource server can apply
	// policies that differ between a human session and a deploy token without
	// a database lookup on the pull path.
	Kind string `json:"mantle_kind,omitempty"`
}

// Issuer mints registry tokens.
type Issuer struct {
	key     *SigningKey
	issuer  string
	service string
	ttl     time.Duration
}

// NewIssuer creates a token issuer. The issuer and service strings appear in
// the `iss` and `aud` claims and must match what the WWW-Authenticate challenge
// advertises, or clients will reject the token they were just handed.
func NewIssuer(key *SigningKey, issuer, service string, ttl time.Duration) *Issuer {
	return &Issuer{key: key, issuer: issuer, service: service, ttl: ttl}
}

// Service returns the configured service name.
func (i *Issuer) Service() string { return i.service }

// TTL returns the token lifetime.
func (i *Issuer) TTL() time.Duration { return i.ttl }

// Key exposes the signing key so the daemon can publish JWKS.
func (i *Issuer) Key() *SigningKey { return i.key }

// IssueParams describes a token to mint.
type IssueParams struct {
	Subject string
	Kind    string
	Access  []Access
}

// Issued is a minted token and the metadata clients expect alongside it.
type Issued struct {
	Token     string
	TokenID   string
	IssuedAt  time.Time
	ExpiresIn int
}

// Issue mints a signed token.
//
// The `nbf` claim is backdated by a small skew allowance. Container hosts
// frequently have clocks a second or two out, and a token rejected as
// not-yet-valid produces a deploy failure whose cause is entirely invisible
// from the client side — which is why preflight also checks clock drift.
const clockSkewAllowance = 30 * time.Second

func (i *Issuer) Issue(p IssueParams) (*Issued, error) {
	now := time.Now()
	jti, err := randomID()
	if err != nil {
		return nil, err
	}

	// An empty access array is meaningful and must serialise as [] rather than
	// null: it is the correct answer to "authenticate me for nothing in
	// particular", which is what `docker login` does.
	access := p.Access
	if access == nil {
		access = []Access{}
	}

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			Subject:   p.Subject,
			Audience:  jwt.ClaimStrings{i.service},
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
			NotBefore: jwt.NewNumericDate(now.Add(-clockSkewAllowance)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        jti,
		},
		Access: access,
		Kind:   p.Kind,
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = i.key.KeyID
	signed, err := tok.SignedString(i.key.Private)
	if err != nil {
		return nil, fmt.Errorf("signing registry token: %w", err)
	}
	return &Issued{
		Token:     signed,
		TokenID:   jti,
		IssuedAt:  now,
		ExpiresIn: int(i.ttl.Seconds()),
	}, nil
}

// Verifier validates presented tokens.
type Verifier struct {
	key     *SigningKey
	issuer  string
	service string
	// revoked reports whether a token id has been revoked before expiry. It is
	// consulted only for machine identities, whose jti values are recorded
	// (§9.1); human sessions rely on the short TTL.
	revoked func(jti string) bool
}

// NewVerifier creates a verifier.
func NewVerifier(key *SigningKey, issuer, service string) *Verifier {
	return &Verifier{key: key, issuer: issuer, service: service}
}

// SetRevocationCheck installs a revocation predicate.
func (v *Verifier) SetRevocationCheck(fn func(jti string) bool) { v.revoked = fn }

// Verify parses and validates a bearer token.
//
// The signing method is pinned to RS256. Accepting whatever the token's header
// declares is the classic JWT vulnerability: a token with alg "none", or an
// HMAC token verified against the RSA public key as if it were a shared secret,
// would both authenticate as anyone.
func (v *Verifier) Verify(raw string) (*Claims, error) {
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return &v.key.Private.PublicKey, nil
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.service),
		jwt.WithLeeway(clockSkewAllowance),
	)
	if err != nil {
		return nil, fmt.Errorf("token is not valid: %w", err)
	}
	if !parsed.Valid {
		return nil, fmt.Errorf("token is not valid")
	}
	if v.revoked != nil && claims.ID != "" && v.revoked(claims.ID) {
		return nil, fmt.Errorf("token has been revoked")
	}
	return claims, nil
}

// Allows reports whether the token's access claim permits an action on a
// repository.
//
// The access claim is authoritative and is re-checked on every request
// (SEC-09, REQ-AUTHZ-02). The token is not a capability that was decided once
// at login — it is a statement of what was granted, and the resource server
// still tests it against what is being asked for now.
func (c *Claims) Allows(resourceType, name, action string) bool {
	for _, entry := range c.Access {
		if entry.Type != resourceType || entry.Name != name {
			continue
		}
		for _, granted := range entry.Actions {
			if granted == action || granted == "*" {
				return true
			}
		}
	}
	return false
}

// AccessFromScopes converts granted scopes into access claim entries, dropping
// any that ended up granting nothing.
func AccessFromScopes(scopes []authz.Scope) []Access {
	access := make([]Access, 0, len(scopes))
	for _, s := range scopes {
		if len(s.Actions) == 0 {
			continue
		}
		access = append(access, Access{Type: s.Type, Name: s.Name, Actions: s.Actions})
	}
	return access
}

// SubjectIdentityUUID extracts the identity UUID from a subject URN of the form
// "mantle:<kind>:<uuid>".
func SubjectIdentityUUID(subject string) (string, bool) {
	parts := strings.Split(subject, ":")
	if len(parts) != 3 || parts[0] != "mantle" {
		return "", false
	}
	return parts[2], true
}

func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating token id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
