// Package iamauth wires the nsc-teamusers Go SDK into dispatchub: bearer-token
// verification, the any → team → own scope ladder over the dispatch:* keys,
// the client-credentials service token used for outbound calls, and the
// permission catalog the register-permissions subcommand upserts.
package iamauth

import (
	"context"
	"net/http"

	iam "github.com/crazy4chicken/nsc-teamusers/sdk/go"
)

// Scope is the resource identity the permission ladder resolves a request
// against: the owning team and user of the room or session being accessed. An
// empty field means that rung does not apply to the resource.
type Scope struct {
	TeamID  string
	OwnerID string
}

// ScopeResolver loads the resource a route is authorized against. It runs
// before the permission decision, so it must not depend on the request body.
// Returning a *ResourceError aborts the request with its status and detail;
// any other error becomes a 500.
type ScopeResolver func(r *http.Request) (Scope, error)

// ResourceError is a resolver failure that maps onto an RFC 9457 problem
// response. Detail carries the stable API error code; Err keeps the cause for
// the log without ever reaching the client.
type ResourceError struct {
	Status int
	Detail string
	Err    error
}

// Error implements error.
func (e *ResourceError) Error() string {
	if e == nil {
		return "iam: resource resolution failed"
	}
	if e.Err != nil {
		return "iam: " + e.Detail + ": " + e.Err.Error()
	}
	return "iam: " + e.Detail
}

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *ResourceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Claims is the identity information dispatchub reads from a verified token:
// the subject (used for audit and own-scoped access), the team (team-scoped
// access) and the subject kind.
type Claims struct {
	Subject string
	Team    string
	Kind    string
}

// ClaimsFromContext returns the claims the Middleware stored for the request.
// It reports false when the request carries no verified identity.
func ClaimsFromContext(ctx context.Context) (Claims, bool) {
	claims, ok := iam.ClaimsFromContext(ctx)
	if !ok {
		return Claims{}, false
	}
	return Claims{Subject: claims.Subject, Team: claims.Team, Kind: claims.Kind}, true
}

// permissionLadder builds the candidate permission keys for one action and the
// resource they are evaluated against. The broadest scope is always tried;
// team is added only when the resource belongs to the caller's team and own
// only when the caller owns the resource, mirroring plan §5.
func permissionLadder(action string, scope Scope, claims iam.Claims) ([]string, iam.Resource) {
	keys := []string{resourceSegment + ":" + action + ":" + scopeAny}
	if scope.TeamID != "" && claims.Team == scope.TeamID {
		keys = append(keys, resourceSegment+":"+action+":"+scopeTeam)
	}
	if scope.OwnerID != "" && claims.Subject == scope.OwnerID {
		keys = append(keys, resourceSegment+":"+action+":"+scopeOwn)
	}
	return keys, iam.Resource{OwnerID: scope.OwnerID, TeamID: scope.TeamID}
}
