package gateway

import "net/http"

// IsAdmin reads only the authenticated context, never caller-supplied headers.
// A bare handler has no authentication layer, just like auth-disabled mode;
// AuthMiddleware always attaches an owner identity when authentication is off.
func IsAdmin(r *http.Request) bool {
	identity, ok := IdentityFromContext(r.Context())
	return !ok || roleRank(identity.Role) >= roleRank("admin")
}

// ScopedAPIKeyID prevents a non-admin identity without a local key (for example
// OIDC with no subject) from accidentally falling through to a global query.
func ScopedAPIKeyID(r *http.Request) string {
	identity, ok := IdentityFromContext(r.Context())
	if !ok || roleRank(identity.Role) >= roleRank("admin") {
		return ""
	}
	if identity.APIKeyID != "" {
		return identity.APIKeyID
	}
	return "__omniswitch_no_principal__"
}

// EffectiveRole normalizes a stored legacy role to its actual auth privilege.
func EffectiveRole(role string) string { return normalizeRole(role) }
