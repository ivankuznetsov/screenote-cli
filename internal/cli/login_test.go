package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appconfig "github.com/ivankuznetsov/screenote-cli/internal/config"
)

func TestRunLoginRegistersOpensBrowserAndStoresTokenResponse(t *testing.T) {
	originalOpenBrowser := openBrowser
	defer func() { openBrowser = originalOpenBrowser }()

	var openedURL string
	var authorizeQuery url.Values
	openBrowser = func(rawURL string) error {
		openedURL = rawURL
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		authorizeQuery = parsed.Query()
		go func() {
			redirectURI := authorizeQuery.Get("redirect_uri")
			_, _ = http.Get(redirectURI + "?state=" + url.QueryEscape(authorizeQuery.Get("state")) + "&code=code-1")
		}()
		return nil
	}

	var registeredRedirect string
	var tokenForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":                 serverURL(r),
				"authorization_endpoint": serverURL(r) + "/oauth/authorize",
				"token_endpoint":         serverURL(r) + "/oauth/token",
				"registration_endpoint":  serverURL(r) + "/oauth/register",
			})
		case "/oauth/register":
			var payload struct {
				RedirectURIs []string `json:"redirect_uris"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			registeredRedirect = payload.RedirectURIs[0]
			_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "client-1"})
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			tokenForm = r.Form
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "access-1",
				"refresh_token": "refresh-1",
				"expires_in":    3600,
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	a := &app{stderr: &stderr, httpClient: server.Client()}
	credentials, err := a.runLogin(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if openedURL == "" || authorizeQuery.Get("scope") != oauthScope || authorizeQuery.Get("code_challenge_method") != "S256" {
		t.Fatalf("openedURL=%q query=%v", openedURL, authorizeQuery)
	}
	if registeredRedirect == "" || authorizeQuery.Get("redirect_uri") != registeredRedirect {
		t.Fatalf("registered=%q authorize redirect=%q", registeredRedirect, authorizeQuery.Get("redirect_uri"))
	}
	if tokenForm.Get("grant_type") != "authorization_code" || tokenForm.Get("code") != "code-1" || tokenForm.Get("code_verifier") == "" {
		t.Fatalf("tokenForm=%v", tokenForm)
	}
	if credentials.AccessToken != "access-1" || credentials.RefreshToken != "refresh-1" || credentials.ClientID != "client-1" || credentials.BaseURL != server.URL {
		t.Fatalf("credentials=%#v", credentials)
	}
}

func TestCallbackRejectsMismatchedState(t *testing.T) {
	result := make(chan callbackResult, 1)
	server := httptest.NewServer(callbackHandler("expected", result))
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/callback?state=wrong&code=abc")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	received := <-result
	if received.err == nil || !strings.Contains(received.err.Error(), "state") {
		t.Fatalf("result=%#v", received)
	}
}

func TestRunLoginBrowserFallbackUsesJSONStderr(t *testing.T) {
	originalOpenBrowser := openBrowser
	defer func() { openBrowser = originalOpenBrowser }()

	ctx, cancel := context.WithCancel(context.Background())
	openBrowser = func(rawURL string) error {
		cancel()
		return errors.New("no browser")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":                 serverURL(r),
				"authorization_endpoint": serverURL(r) + "/oauth/authorize",
				"token_endpoint":         serverURL(r) + "/oauth/token",
				"registration_endpoint":  serverURL(r) + "/oauth/register",
			})
		case "/oauth/register":
			_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "client-1"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	var stderr bytes.Buffer
	a := &app{stderr: &stderr, httpClient: server.Client()}
	_, err := a.runLogin(ctx, server.URL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
		t.Fatalf("stderr=%q err=%v", stderr.String(), err)
	}
	if payload["code"] != "browser_open_failed" || payload["authorization_url"] == "" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestSaveLoginCredentialsSwitchesConfiguredBaseURL(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := appconfig.Save(configPath, appconfig.Values{BaseURL: "https://old.example"}); err != nil {
		t.Fatal(err)
	}

	a := &app{configPath: configPath}
	credentials := &appconfig.LoginCredentials{
		AccessToken: "access-1",
		ClientID:    "client-1",
		BaseURL:     "https://new.example",
	}
	path, err := a.saveLoginCredentials("https://new.example", credentials)
	if err != nil {
		t.Fatal(err)
	}
	if path != configPath {
		t.Fatalf("path=%q", path)
	}
	values, err := appconfig.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if values.BaseURL != "https://new.example" || values.Login == nil || values.Login.AccessToken != "access-1" {
		t.Fatalf("values=%#v", values)
	}
}

func TestStoredExpiredCredentialsRefreshBeforeCommand(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	var refreshed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"authorization_endpoint": serverURL(r) + "/oauth/authorize",
				"token_endpoint":         serverURL(r) + "/oauth/token",
				"registration_endpoint":  serverURL(r) + "/oauth/register",
			})
		case "/oauth/token":
			refreshed = true
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-1" {
				t.Fatalf("form=%v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "access-2",
				"refresh_token": "refresh-2",
				"expires_in":    3600,
			})
		case "/api/v1/projects":
			if r.Header.Get("Authorization") != "Bearer access-2" {
				t.Fatalf("Authorization=%q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"projects":[]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	err := appconfig.Save(configPath, appconfig.Values{
		BaseURL: server.URL,
		Login: &appconfig.LoginCredentials{
			AccessToken:  "access-1",
			RefreshToken: "refresh-1",
			ExpiresAt:    time.Now().Add(-time.Hour),
			ClientID:     "client-1",
			BaseURL:      server.URL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLI(t, []string{"--config", configPath, "project", "list"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !refreshed {
		t.Fatal("expected refresh request")
	}
	values, err := appconfig.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if values.Login.AccessToken != "access-2" || values.Login.RefreshToken != "refresh-2" {
		t.Fatalf("login=%#v", values.Login)
	}
}

func TestStoredExpiredCredentialsFailedRefreshReturnsAuthError(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"authorization_endpoint": serverURL(r) + "/oauth/authorize",
				"token_endpoint":         serverURL(r) + "/oauth/token",
				"registration_endpoint":  serverURL(r) + "/oauth/register",
			})
		case "/oauth/token":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	err := appconfig.Save(configPath, appconfig.Values{
		BaseURL: server.URL,
		Login: &appconfig.LoginCredentials{
			AccessToken:  "access-1",
			RefreshToken: "refresh-1",
			ExpiresAt:    time.Now().Add(-time.Hour),
			ClientID:     "client-1",
			BaseURL:      server.URL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLI(t, []string{"--config", configPath, "project", "list"}, "")
	if code != ExitAuth {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "invalid_token") {
		t.Fatalf("stderr=%s", stderr)
	}
}

func TestExplicitTokenSkipsStoredRefresh(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			t.Fatal("unexpected refresh")
		}
		if r.URL.Path != "/api/v1/projects" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer explicit" {
			t.Fatalf("Authorization=%q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"projects":[]}`))
	}))
	defer server.Close()

	err := appconfig.Save(configPath, appconfig.Values{
		BaseURL: server.URL,
		Login: &appconfig.LoginCredentials{
			AccessToken:  "access-1",
			RefreshToken: "refresh-1",
			ExpiresAt:    time.Now().Add(-time.Hour),
			ClientID:     "client-1",
			BaseURL:      server.URL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLI(t, []string{"--config", configPath, "--token", "explicit", "project", "list"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestLogoutRemovesStoredCredentials(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := appconfig.Save(configPath, appconfig.Values{
		Token:   "explicit",
		BaseURL: "http://example.test",
		Login:   &appconfig.LoginCredentials{AccessToken: "stored"},
	}); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLI(t, []string{"--config", configPath, "logout"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	values, err := appconfig.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if values.Login != nil || values.Token != "explicit" || values.BaseURL != "http://example.test" {
		t.Fatalf("values=%#v", values)
	}
}

func serverURL(r *http.Request) string {
	u := url.URL{Scheme: "http", Host: r.Host}
	return u.String()
}
