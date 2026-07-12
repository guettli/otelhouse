package tenantauth

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// saIdentity is the ServiceAccount a verified projected token belongs to.
// Both fields come from claims the API server signed; nothing here is
// client-supplied.
type saIdentity struct {
	namespace string
	name      string
}

func (i saIdentity) String() string { return i.namespace + "/" + i.name }

// serviceAccountVerifier verifies Kubernetes projected ServiceAccount
// tokens against the cluster JWKS and maps the verified identity onto a
// tenant.
//
// Everything a decision depends on — algorithm, issuer, audience,
// namespace, ServiceAccount name — comes from the signed token. The only
// client-controlled input is the token itself.
type serviceAccountVerifier struct {
	cfg    *ServiceAccountConfig
	keys   *jwksCache
	parser *jwt.Parser

	// allowedAlgs is the pinned set, also re-checked inside the keyfunc as
	// defence in depth against a parser regression.
	allowedAlgs map[string]struct{}
	// namespaces is the optional allowlist for namespace-as-tenant.
	namespaces map[string]struct{}
}

func newServiceAccountVerifier(cfg *ServiceAccountConfig) (*serviceAccountVerifier, error) {
	fetch := cfg.fetchJWKS
	if fetch == nil {
		var err error
		if fetch, err = newHTTPJWKSFetcher(cfg); err != nil {
			return nil, err
		}
	}
	algs := cfg.algorithms()
	allowed := make(map[string]struct{}, len(algs))
	for _, a := range algs {
		if _, ok := supportedAlgorithms[a]; !ok {
			// Belt and braces: validate() already refused this.
			return nil, fmt.Errorf("tenantauth: serviceaccount: unsupported algorithm %q", a)
		}
		allowed[a] = struct{}{}
	}
	ns := make(map[string]struct{}, len(cfg.Namespaces))
	for _, n := range cfg.Namespaces {
		ns[n] = struct{}{}
	}
	return &serviceAccountVerifier{
		cfg:  cfg,
		keys: newJWKSCache(fetch, cfg.minRefreshInterval()),
		parser: jwt.NewParser(
			// Pin the algorithms: refuses `alg:none` and blocks HS↔RS/ES
			// confusion. HS* can never appear here — validate() rejects it
			// at config load.
			jwt.WithValidMethods(algs),
			jwt.WithIssuer(cfg.Issuer),
			// The audience the producer had to project the token with. A
			// token minted for the API server carries `aud:
			// https://kubernetes.default.svc...` and is NOT replayable here.
			jwt.WithAudience(cfg.audience()),
			jwt.WithExpirationRequired(),
		),
		allowedAlgs: allowed,
		namespaces:  ns,
	}, nil
}

func (v *serviceAccountVerifier) name() string { return "serviceaccount" }

// issuer is what the chain uses to decide which verifier "owns" a token, so
// the rejection reason reported to the operator is the interesting one.
func (v *serviceAccountVerifier) issuer() string { return v.cfg.Issuer }

// warmup pre-populates the key cache at extension start, so the first real
// request does not pay for the JWKS fetch. A failure here is not fatal: the
// cluster's API server may not be reachable yet at startup, and the fetch is
// retried (rate-limited) on the first token that needs a key. Fail-closed is
// preserved — until keys are known, tokens are rejected, never accepted.
func (v *serviceAccountVerifier) warmup(ctx context.Context) error {
	return v.keys.refresh(ctx)
}

// verify checks a token and resolves the tenant it may write as.
func (v *serviceAccountVerifier) verify(ctx context.Context, token string) (string, *rejection) {
	claims := jwt.MapClaims{}
	parsed, err := v.parser.ParseWithClaims(token, claims, func(tok *jwt.Token) (any, error) {
		alg, _ := tok.Header["alg"].(string)
		if tok.Method == nil || alg == "" || tok.Method.Alg() != alg {
			return nil, fmt.Errorf("%w: %v", errAlgNotAllowed, tok.Header["alg"])
		}
		// Re-check the pin here so a future jwt-go bug cannot relax
		// WithValidMethods behind our back. HS*/none never reach a key.
		if _, ok := v.allowedAlgs[alg]; !ok {
			return nil, fmt.Errorf("%w: %s", errAlgNotAllowed, alg)
		}
		kid, _ := tok.Header["kid"].(string)
		key, err := v.keys.keyFor(ctx, kid, alg)
		if err != nil {
			return nil, err
		}
		if set, ok := key.([]crypto.PublicKey); ok {
			keys := make([]jwt.VerificationKey, 0, len(set))
			for _, k := range set {
				keys = append(keys, k)
			}
			return jwt.VerificationKeySet{Keys: keys}, nil
		}
		return key, nil
	})
	if err != nil {
		return "", &rejection{
			reason:     classifySAError(err),
			err:        fmt.Errorf("tenantauth: serviceaccount token: %w", err),
			recognized: issuerMatches(claims, v.cfg.Issuer),
		}
	}
	if parsed == nil || !parsed.Valid {
		return "", &rejection{reason: reasonMalformed, err: errors.New("tenantauth: serviceaccount token invalid")}
	}

	id, err := serviceAccountIdentity(claims)
	if err != nil {
		return "", &rejection{reason: reasonUnmappedSA, err: err, recognized: true}
	}
	tenant, err := v.tenantFor(id)
	if err != nil {
		return "", &rejection{reason: reasonUnmappedSA, err: err, recognized: true}
	}
	return tenant, nil
}

// tenantFor maps a verified ServiceAccount identity onto a tenant.
//
// Precedence:
//  1. an explicit tenant_map entry for "<namespace>/<serviceaccount>";
//  2. the namespace itself, if namespace_as_tenant is on and the namespace
//     passes the optional allowlist.
//
// Anything else is REJECTED. There is deliberately no fallback tenant: a
// batch that cannot be attributed to a tenant by a verified claim must not
// be written at all, because every other tenant's row-policy isolation
// depends on the label being unforgeable.
func (v *serviceAccountVerifier) tenantFor(id saIdentity) (string, error) {
	if tenant, ok := v.cfg.TenantMap[id.String()]; ok {
		return tenant, nil
	}
	if !v.cfg.NamespaceAsTenant {
		return "", fmt.Errorf("tenantauth: serviceaccount %q is not in tenant_map (namespace_as_tenant is off)", id)
	}
	if len(v.namespaces) > 0 {
		if _, ok := v.namespaces[id.namespace]; !ok {
			return "", fmt.Errorf("tenantauth: namespace %q of serviceaccount %q is not an allowed tenant namespace and has no tenant_map entry", id.namespace, id)
		}
	}
	return id.namespace, nil
}

// serviceAccountIdentity pulls the namespace + ServiceAccount name out of
// the *verified* claims of a projected token.
//
// Projected (bound) tokens carry a nested object:
//
//	"kubernetes.io": {"namespace": "ns", "serviceaccount": {"name": "sa", ...}}
//
// Legacy (Secret-based) tokens carry flat claims instead:
//
//	"kubernetes.io/serviceaccount/namespace": "ns"
//	"kubernetes.io/serviceaccount/service-account.name": "sa"
//
// Both are read, and `sub` ("system:serviceaccount:<ns>:<name>") is the last
// resort, so the extension works across token flavours. All three come from
// the same signature.
func serviceAccountIdentity(claims jwt.MapClaims) (saIdentity, error) {
	if id, ok := identityFromNested(claims); ok {
		return id, nil
	}
	if id, ok := identityFromFlatClaims(claims); ok {
		return id, nil
	}
	if id, ok := identityFromSubject(claims); ok {
		return id, nil
	}
	return saIdentity{}, errors.New("tenantauth: token carries no Kubernetes ServiceAccount identity (no kubernetes.io claims, no system:serviceaccount subject)")
}

func identityFromNested(claims jwt.MapClaims) (saIdentity, bool) {
	k8s, ok := claims["kubernetes.io"].(map[string]any)
	if !ok {
		return saIdentity{}, false
	}
	ns, _ := k8s["namespace"].(string)
	sa, _ := k8s["serviceaccount"].(map[string]any)
	name, _ := sa["name"].(string)
	return newIdentity(ns, name)
}

func identityFromFlatClaims(claims jwt.MapClaims) (saIdentity, bool) {
	ns, _ := claims["kubernetes.io/serviceaccount/namespace"].(string)
	name, _ := claims["kubernetes.io/serviceaccount/service-account.name"].(string)
	return newIdentity(ns, name)
}

func identityFromSubject(claims jwt.MapClaims) (saIdentity, bool) {
	sub, _ := claims["sub"].(string)
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(sub, prefix) {
		return saIdentity{}, false
	}
	ns, name, ok := strings.Cut(strings.TrimPrefix(sub, prefix), ":")
	if !ok {
		return saIdentity{}, false
	}
	return newIdentity(ns, name)
}

// newIdentity rejects empty or whitespace-only components: an identity that
// is not fully specified is not an identity, and must never become a tenant.
func newIdentity(namespace, name string) (saIdentity, bool) {
	namespace, name = strings.TrimSpace(namespace), strings.TrimSpace(name)
	if namespace == "" || name == "" {
		return saIdentity{}, false
	}
	return saIdentity{namespace: namespace, name: name}, true
}

// classifySAError maps a verification failure to a `reason` label. The
// JWKS-specific sentinels are checked first; everything else falls through
// to the shared JWT classifier (expired / bad_audience / bad_issuer /
// bad_signature / malformed).
func classifySAError(err error) string {
	switch {
	case errors.Is(err, errUnknownKID):
		return reasonUnknownKID
	case errors.Is(err, errJWKSUnavailable), errors.Is(err, errNoKeys):
		return reasonJWKSUnavailable
	case errors.Is(err, errAlgNotAllowed):
		return reasonBadSignature
	default:
		return classifyJWTError(err)
	}
}

// issuerMatches reports whether the token claims to come from this
// verifier's issuer. Used only to pick which verifier's rejection reason to
// report when several are enabled — never to grant anything.
func issuerMatches(claims jwt.MapClaims, issuer string) bool {
	iss, _ := claims["iss"].(string)
	return iss != "" && iss == issuer
}
