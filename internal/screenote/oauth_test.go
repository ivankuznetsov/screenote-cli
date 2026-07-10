package screenote

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestAuthorizationURLUsesPKCEStateAndScopes(t *testing.T) {
	rawURL, err := AuthorizationURL(OAuthMetadata{AuthorizationEndpoint: "https://screenote.test/oauth/authorize"}, "client-1", "http://127.0.0.1:1234/callback", "verifier", "state-1", "mcp_read mcp_write")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("client_id") != "client-1" || query.Get("redirect_uri") != "http://127.0.0.1:1234/callback" || query.Get("state") != "state-1" {
		t.Fatalf("query=%s", parsed.RawQuery)
	}
	if query.Get("scope") != "mcp_read mcp_write" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("query=%s", parsed.RawQuery)
	}
	sum := sha256.Sum256([]byte("verifier"))
	if query.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatalf("code_challenge=%q", query.Get("code_challenge"))
	}
}

func TestOAuthRefreshPostsForm(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "client-1" || r.Form.Get("refresh_token") != "refresh-1" {
			t.Fatalf("form=%v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-2",
			"refresh_token": "refresh-2",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	response, err := RefreshAccessToken(context.Background(), OAuthMetadata{TokenEndpoint: server.URL + "/oauth/token"}, "client-1", "refresh-1", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if response.AccessToken != "access-2" || response.RefreshToken != "refresh-2" {
		t.Fatalf("response=%#v", response)
	}
}
