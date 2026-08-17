package authn

import "strings"

// The identity namespace.
//
// An RBAC binding names a subject as a bare string, and that string has to mean exactly one person
// forever. OIDC Core §5.7 is blunt about which claim can carry that promise: sub is the only one
// that is stable and unique inside an issuer, and email/preferred_username "MUST NOT be used as
// unique identifiers" because an IdP may reassign them. Grafana keyed identity on email and got
// CVE-2023-3128 (CVSS 9.4) for it: take a leaver's address, inherit their roles.
//
// So an OIDC identity here is "oidc:" + sub. The prefix is not decoration:
//
//   - It says WHICH namespace the opaque half came from, so a sub that happens to look like a local
//     user's UUID cannot resolve that user's bindings.
//   - It makes a binding readable as what it is. A row saying subject_id = "8f0e...-4b21" is not
//     reviewable; "oidc:8f0e...-4b21" is.
//   - It gives the login path something to REFUSE. An issuer that mints sub = "local:<uuid>" would
//     otherwise hand its holder every binding that local user has; see reservedIdentity.
//
// The other three prefixes are reserved rather than used: local mode binds users by their users.id
// UUID and token/header identities are their own strings today. Reserving them now costs nothing
// and keeps the namespace free for those modes to adopt the same shape later.
const (
	// IdentityPrefixOIDC prefixes an OIDC subject's sub claim.
	IdentityPrefixOIDC = "oidc:"
	// IdentityPrefixLocal is reserved for database users (bound by users.id today).
	IdentityPrefixLocal = "local:"
	// IdentityPrefixHeader is reserved for proxy-asserted identities.
	IdentityPrefixHeader = "header:"
	// IdentityPrefixToken is reserved for API tokens (bound by tokens.id today).
	IdentityPrefixToken = "token:"
)

// reservedPrefixes is the closed set reservedIdentity checks against.
var reservedPrefixes = []string{
	IdentityPrefixOIDC,
	IdentityPrefixLocal,
	IdentityPrefixHeader,
	IdentityPrefixToken,
}

// reservedIdentity reports whether an identity string an EXTERNAL party supplied — an IdP's sub, a
// proxy's user header — is trying to sit inside a namespace this console mints itself.
//
// Nothing legitimate does this: a real sub is opaque to its issuer's own namespace, and a proxy
// asserts a username. What it stops is an issuer or a compromised proxy claiming to be an identity
// from another mode, which is the one way the prefix scheme could be turned inside out.
func reservedIdentity(raw string) bool {
	for _, prefix := range reservedPrefixes {
		if strings.HasPrefix(raw, prefix) {
			return true
		}
	}
	return false
}
