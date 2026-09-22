package tenantauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPJWKSFetcherAcceptsJWKSMediaType guards against regressing to
// Accept: application/json. The Kubernetes /openid/v1/jwks endpoint serves the
// RFC 7517 media type application/jwk-set+json and answers a bare
// application/json Accept with 406 Not Acceptable. That failure was invisible
// in production — a 406 became an empty key cache and unknown_kid rejections —
// so this asserts the fetcher requests the JWKS media type against a server
// that mimics the apiserver's content negotiation.
func TestHTTPJWKSFetcherAcceptsJWKSMediaType(t *testing.T) {
	const jwks = `{"keys":[]}`
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		if !strings.Contains(gotAccept, "application/jwk-set+json") {
			http.Error(w, "406", http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Content-Type", "application/jwk-set+json")
		_, _ = w.Write([]byte(jwks))
	}))
	defer srv.Close()

	// http:// URL skips CA loading; noTokenFile skips the bearer token — the
	// test server needs neither, only the Accept header is under test.
	fetch, err := newHTTPJWKSFetcher(&ServiceAccountConfig{
		JWKSURL:   srv.URL,
		TokenFile: noTokenFile,
	})
	if err != nil {
		t.Fatalf("newHTTPJWKSFetcher: %v", err)
	}

	body, err := fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch returned error (server saw Accept=%q): %v", gotAccept, err)
	}
	if string(body) != jwks {
		t.Fatalf("body = %q, want %q", body, jwks)
	}
	if !strings.Contains(gotAccept, "application/jwk-set+json") {
		t.Errorf("Accept header = %q, must request application/jwk-set+json", gotAccept)
	}
}
