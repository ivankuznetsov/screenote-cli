package screenote

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestDiscoverOAuthIncludesDeviceAuthorizationCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/.well-known/oauth-authorization-server" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                        oauthTestServerURL(r),
			"authorization_endpoint":        oauthTestServerURL(r) + "/oauth/authorize",
			"token_endpoint":                oauthTestServerURL(r) + "/oauth/token",
			"registration_endpoint":         oauthTestServerURL(r) + "/oauth/register",
			"device_authorization_endpoint": oauthTestServerURL(r) + "/oauth/authorize_device",
			"grant_types_supported":         []string{"authorization_code", "refresh_token", DeviceCodeGrantType},
		})
	}))
	defer server.Close()

	metadata, err := DiscoverOAuth(context.Background(), server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if metadata.DeviceAuthorizationEndpoint != server.URL+"/oauth/authorize_device" {
		t.Fatalf("device endpoint=%q", metadata.DeviceAuthorizationEndpoint)
	}
	if !metadata.SupportsDeviceAuthorization() || !slices.Contains(metadata.GrantTypesSupported, DeviceCodeGrantType) {
		t.Fatalf("metadata=%#v", metadata)
	}

	metadata.GrantTypesSupported = []string{"authorization_code", "refresh_token"}
	if metadata.SupportsDeviceAuthorization() {
		t.Fatal("device endpoint without the exact device grant must not be treated as supported")
	}
}

func TestRegisterDeviceOAuthClientPostsPublicGrantMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/register" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("headers=%v", r.Header)
		}
		var payload struct {
			ClientName              string   `json:"client_name"`
			RedirectURIs            []string `json:"redirect_uris"`
			GrantTypes              []string `json:"grant_types"`
			TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.ClientName != "Screenote CLI" || len(payload.RedirectURIs) != 1 || payload.RedirectURIs[0] != DeviceOAuthRedirectURI {
			t.Fatalf("payload=%#v", payload)
		}
		if !slices.Equal(payload.GrantTypes, []string{DeviceCodeGrantType, "refresh_token"}) || payload.TokenEndpointAuthMethod != "none" {
			t.Fatalf("payload=%#v", payload)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "device-client"})
	}))
	defer server.Close()

	registration, err := RegisterDeviceOAuthClient(
		context.Background(),
		OAuthMetadata{RegistrationEndpoint: server.URL + "/oauth/register"},
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if registration.ClientID != "device-client" {
		t.Fatalf("registration=%#v", registration)
	}
}

func TestRequestDeviceAuthorizationPostsFormAndDefaultsInterval(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/authorize_device" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("headers=%v", r.Header)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != "device-client" || r.Form.Get("scope") != "mcp_read mcp_write" {
			t.Fatalf("form=%v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "secret-device-code",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          "https://screenote.test/oauth/device",
			"verification_uri_complete": "https://screenote.test/oauth/device?user_code=ABCD-EFGH",
			"expires_in":                600,
		})
	}))
	defer server.Close()

	response, err := RequestDeviceAuthorization(
		context.Background(),
		OAuthMetadata{DeviceAuthorizationEndpoint: server.URL + "/oauth/authorize_device"},
		"device-client",
		"mcp_read mcp_write",
		server.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.DeviceCode != "secret-device-code" || response.UserCode != "ABCD-EFGH" || response.ExpiresIn != 600 {
		t.Fatalf("response=%#v", response)
	}
	if response.Interval != DefaultDevicePollIntervalSeconds {
		t.Fatalf("interval=%d want=%d", response.Interval, DefaultDevicePollIntervalSeconds)
	}
}

func TestRequestDeviceAuthorizationValidatesResponse(t *testing.T) {
	tests := []struct {
		name     string
		response map[string]any
	}{
		{
			name: "missing device code",
			response: map[string]any{
				"user_code": "ABCD-EFGH", "verification_uri": "https://screenote.test/oauth/device", "expires_in": 600,
			},
		},
		{
			name: "missing user code",
			response: map[string]any{
				"device_code": "device", "verification_uri": "https://screenote.test/oauth/device", "expires_in": 600,
			},
		},
		{
			name: "missing verification uri",
			response: map[string]any{
				"device_code": "device", "user_code": "ABCD-EFGH", "expires_in": 600,
			},
		},
		{
			name: "nonpositive expiry",
			response: map[string]any{
				"device_code": "device", "user_code": "ABCD-EFGH", "verification_uri": "https://screenote.test/oauth/device", "expires_in": 0,
			},
		},
		{
			name: "nonpositive interval",
			response: map[string]any{
				"device_code": "device", "user_code": "ABCD-EFGH", "verification_uri": "https://screenote.test/oauth/device", "expires_in": 600, "interval": 0,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(test.response)
			}))
			defer server.Close()

			_, err := RequestDeviceAuthorization(
				context.Background(),
				OAuthMetadata{DeviceAuthorizationEndpoint: server.URL},
				"client",
				"scope",
				server.Client(),
			)
			if err == nil {
				t.Fatal("expected malformed response error")
			}
			if strings.Contains(err.Error(), "secret-device-code") {
				t.Fatalf("error leaked device code: %v", err)
			}
		})
	}
}

func TestExchangeDeviceCodePostsGrantAndReturnsTypedOAuthError(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != DeviceCodeGrantType || r.Form.Get("device_code") != "secret-device-code" || r.Form.Get("client_id") != "device-client" {
			t.Fatalf("form=%v", r.Form)
		}
		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":             "authorization_pending",
				"error_description": "Finish authorizing in your browser",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 3600,
		})
	}))
	defer server.Close()

	metadata := OAuthMetadata{TokenEndpoint: server.URL + "/oauth/token"}
	_, err := ExchangeDeviceCode(context.Background(), metadata, "device-client", "secret-device-code", server.Client())
	var oauthError *OAuthError
	if !errors.As(err, &oauthError) {
		t.Fatalf("err=%T %v", err, err)
	}
	if oauthError.Code != "authorization_pending" || oauthError.Description != "Finish authorizing in your browser" || oauthError.StatusCode != http.StatusBadRequest {
		t.Fatalf("oauth error=%#v", oauthError)
	}

	response, err := ExchangeDeviceCode(context.Background(), metadata, "device-client", "secret-device-code", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if response.AccessToken != "access-1" || response.RefreshToken != "refresh-1" {
		t.Fatalf("response=%#v", response)
	}
}

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

func oauthTestServerURL(r *http.Request) string {
	return (&url.URL{Scheme: "http", Host: r.Host}).String()
}
