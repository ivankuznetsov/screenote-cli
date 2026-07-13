package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appconfig "github.com/ivankuznetsov/screenote-cli/internal/config"
	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
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

func TestLoginDeviceFlagUsesDeviceOAuthWithoutListenerOrBrowser(t *testing.T) {
	originalOpenBrowser := openBrowser
	originalListenLoopback := listenLoopback
	defer func() {
		openBrowser = originalOpenBrowser
		listenLoopback = originalListenLoopback
	}()

	openBrowser = func(string) error {
		t.Fatal("device login must not open a browser")
		return nil
	}
	listenLoopback = func() (net.Listener, error) {
		t.Fatal("device login must not open a loopback listener")
		return nil, errors.New("unexpected listener")
	}

	server := newDeviceLoginServer(t, map[string]any{
		"device_code":      "device-secret",
		"user_code":        "ABCD-EFGH",
		"verification_uri": "https://screenote.test/oauth/device",
		"expires_in":       600,
		"interval":         5,
	}, func(w http.ResponseWriter, r *http.Request, attempt int) {
		if attempt != 1 {
			t.Fatalf("attempt=%d", attempt)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-secret", "refresh_token": "refresh-secret", "expires_in": 3600,
		})
	})
	defer server.Close()

	clock := newDeviceTestClock()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	var stdout, stderr bytes.Buffer
	a := &app{
		stdout:     &stdout,
		stderr:     &stderr,
		httpClient: server.Client(),
		configPath: configPath,
		now:        clock.Now,
		wait:       clock.Wait,
	}
	a.flags.BaseURL = server.URL
	cmd := a.loginCommand()
	cmd.SetArgs([]string{"--device"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(clock.waits, []time.Duration{5 * time.Second}) {
		t.Fatalf("waits=%v", clock.waits)
	}
	var event deviceAuthorizationEvent
	if err := json.Unmarshal(stderr.Bytes(), &event); err != nil {
		t.Fatalf("stderr=%q err=%v", stderr.String(), err)
	}
	if event.Event != "device_authorization" || event.AuthorizationURL != "https://screenote.test/oauth/device" || event.VerificationURI != event.AuthorizationURL || event.UserCode != "ABCD-EFGH" {
		t.Fatalf("event=%#v", event)
	}
	for _, secret := range []string{"device-secret", "access-secret", "refresh-secret"} {
		if strings.Contains(stderr.String(), secret) || strings.Contains(stdout.String(), secret) {
			t.Fatalf("output leaked %q: stdout=%q stderr=%q", secret, stdout.String(), stderr.String())
		}
	}
	if !strings.Contains(stdout.String(), `"ok":true`) || !strings.Contains(stdout.String(), configPath) {
		t.Fatalf("stdout=%q", stdout.String())
	}
	values, err := appconfig.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if values.Login == nil || values.Login.AccessToken != "access-secret" || values.Login.RefreshToken != "refresh-secret" || values.Login.ClientID != "device-client" {
		t.Fatalf("values=%#v", values)
	}
}

func TestRunDeviceLoginUsesCompleteURLAndRFC8628PollingIntervals(t *testing.T) {
	clock := newDeviceTestClock()
	server := newDeviceLoginServer(t, map[string]any{
		"device_code":               "device-secret",
		"user_code":                 "ABCD-EFGH",
		"verification_uri":          "https://screenote.test/oauth/device",
		"verification_uri_complete": "https://screenote.test/oauth/device?user_code=ABCD-EFGH",
		"expires_in":                600,
		"interval":                  5,
	}, func(w http.ResponseWriter, r *http.Request, attempt int) {
		if len(clock.waits) != attempt {
			t.Fatalf("token attempt %d happened before wait %d: waits=%v", attempt, attempt, clock.waits)
		}
		switch attempt {
		case 1:
			writeDeviceOAuthError(w, "authorization_pending", "not approved yet")
		case 2:
			writeDeviceOAuthError(w, "slow_down", "poll more slowly")
		case 3:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-secret", "refresh_token": "refresh-secret", "expires_in": 3600,
			})
		default:
			t.Fatalf("unexpected token attempt %d", attempt)
		}
	})
	defer server.Close()

	var stderr bytes.Buffer
	a := &app{stderr: &stderr, httpClient: server.Client(), now: clock.Now, wait: clock.Wait}
	credentials, err := a.runDeviceLogin(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(clock.waits, []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}) {
		t.Fatalf("waits=%v", clock.waits)
	}
	if credentials.AccessToken != "access-secret" || credentials.RefreshToken != "refresh-secret" || credentials.ExpiresAt.Sub(clock.start) != 3620*time.Second {
		t.Fatalf("credentials=%#v now=%v", credentials, clock.Now())
	}
	var event deviceAuthorizationEvent
	if err := json.Unmarshal(stderr.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.AuthorizationURL != "https://screenote.test/oauth/device?user_code=ABCD-EFGH" || event.ExpiresIn != 600 || event.Interval != 5 {
		t.Fatalf("event=%#v", event)
	}
}

func TestRunDeviceLoginNeverPrintsDeviceCodeFromUntrustedOAuthText(t *testing.T) {
	clock := newDeviceTestClock()
	deviceCode := "device/secret"
	server := newDeviceLoginServer(t, map[string]any{
		"device_code":               deviceCode,
		"user_code":                 "ABCD-EFGH",
		"verification_uri":          "https://screenote.test/oauth/device",
		"verification_uri_complete": "https://screenote.test/oauth/device?device_code=" + url.QueryEscape(deviceCode),
		"expires_in":                600,
		"interval":                  5,
	}, func(w http.ResponseWriter, r *http.Request, attempt int) {
		writeDeviceOAuthError(w, "access_denied", "authorization denied for "+deviceCode)
	})
	defer server.Close()

	var stderr bytes.Buffer
	a := &app{stderr: &stderr, httpClient: server.Client(), now: clock.Now, wait: clock.Wait}
	_, err := a.runDeviceLogin(context.Background(), server.URL)
	if err == nil {
		t.Fatal("expected terminal OAuth error")
	}
	if strings.Contains(stderr.String(), deviceCode) || strings.Contains(stderr.String(), url.QueryEscape(deviceCode)) {
		t.Fatalf("stderr leaked device code: %q", stderr.String())
	}
	if strings.Contains(err.Error(), deviceCode) || strings.Contains(err.Error(), url.QueryEscape(deviceCode)) {
		t.Fatalf("error leaked device code: %q", err)
	}

	var event deviceAuthorizationEvent
	if decodeErr := json.Unmarshal(stderr.Bytes(), &event); decodeErr != nil {
		t.Fatalf("stderr=%q err=%v", stderr.String(), decodeErr)
	}
	if event.AuthorizationURL != "https://screenote.test/oauth/device" {
		t.Fatalf("unsafe complete URI was not replaced: %#v", event)
	}
}

func TestSafeDeviceAuthorizationEventRejectsDeviceCodeInVerificationDetails(t *testing.T) {
	deviceCode := "device/secret"
	encodedDeviceCode := url.QueryEscape(deviceCode)
	tests := []struct {
		name            string
		verificationURI string
		userCode        string
	}{
		{
			name:            "raw device code in verification URI",
			verificationURI: "https://screenote.test/oauth/device/" + deviceCode,
			userCode:        "ABCD-EFGH",
		},
		{
			name:            "escaped device code in verification URI",
			verificationURI: "https://screenote.test/oauth/device/" + encodedDeviceCode,
			userCode:        "ABCD-EFGH",
		},
		{
			name:            "raw device code in user code",
			verificationURI: "https://screenote.test/oauth/device",
			userCode:        deviceCode,
		},
		{
			name:            "escaped device code in user code",
			verificationURI: "https://screenote.test/oauth/device",
			userCode:        encodedDeviceCode,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, err := safeDeviceAuthorizationEvent(screenote.DeviceAuthorizationResponse{
				DeviceCode:      deviceCode,
				UserCode:        test.userCode,
				VerificationURI: test.verificationURI,
				ExpiresIn:       600,
				Interval:        5,
			})
			var cliErr *cliError
			if !errors.As(err, &cliErr) || cliErr.Code != "invalid_device_authorization_response" || cliErr.Exit != ExitAuth {
				t.Fatalf("err=%T %#v", err, err)
			}
			if event != (deviceAuthorizationEvent{}) {
				t.Fatalf("event=%#v", event)
			}
			if strings.Contains(err.Error(), deviceCode) || strings.Contains(err.Error(), encodedDeviceCode) {
				t.Fatalf("error leaked device code: %q", err)
			}
		})
	}
}

func TestRunDeviceLoginBacksOffAfterTransportTimeouts(t *testing.T) {
	clock := newDeviceTestClock()
	server := newDeviceLoginServer(t, map[string]any{
		"device_code":      "device-secret",
		"user_code":        "ABCD-EFGH",
		"verification_uri": "https://screenote.test/oauth/device",
		"expires_in":       600,
		"interval":         5,
	}, func(w http.ResponseWriter, r *http.Request, attempt int) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-secret", "expires_in": 3600})
	})
	defer server.Close()

	client := server.Client()
	transport := client.Transport
	tokenAttempts := 0
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/oauth/token" {
			tokenAttempts++
			if tokenAttempts <= 2 {
				return nil, context.DeadlineExceeded
			}
		}
		return transport.RoundTrip(request)
	})

	a := &app{stderr: &bytes.Buffer{}, httpClient: client, now: clock.Now, wait: clock.Wait}
	if _, err := a.runDeviceLogin(context.Background(), server.URL); err != nil {
		t.Fatal(err)
	}
	if tokenAttempts != 3 {
		t.Fatalf("tokenAttempts=%d", tokenAttempts)
	}
	if !slices.Equal(clock.waits, []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}) {
		t.Fatalf("waits=%v", clock.waits)
	}
}

func TestRunDeviceLoginStopsOnOAuthErrors(t *testing.T) {
	tests := []string{"access_denied", "expired_token", "invalid_grant"}
	for _, errorCode := range tests {
		t.Run(errorCode, func(t *testing.T) {
			clock := newDeviceTestClock()
			attempts := 0
			server := newDeviceLoginServer(t, map[string]any{
				"device_code":      "device-secret",
				"user_code":        "ABCD-EFGH",
				"verification_uri": "https://screenote.test/oauth/device",
				"expires_in":       600,
				"interval":         5,
			}, func(w http.ResponseWriter, r *http.Request, attempt int) {
				attempts++
				writeDeviceOAuthError(w, errorCode, "terminal device authorization error")
			})
			defer server.Close()

			a := &app{stderr: &bytes.Buffer{}, httpClient: server.Client(), now: clock.Now, wait: clock.Wait}
			_, err := a.runDeviceLogin(context.Background(), server.URL)
			var cliErr *cliError
			if !errors.As(err, &cliErr) || cliErr.Code != errorCode || cliErr.Exit != ExitAuth {
				t.Fatalf("err=%T %#v", err, err)
			}
			if attempts != 1 || !slices.Equal(clock.waits, []time.Duration{5 * time.Second}) {
				t.Fatalf("attempts=%d waits=%v", attempts, clock.waits)
			}
		})
	}
}

func TestDeviceLoginCommandSerializesTerminalFailureWithoutSecrets(t *testing.T) {
	clock := newDeviceTestClock()
	deviceCode := "device/secret"
	server := newDeviceLoginServer(t, map[string]any{
		"device_code":      deviceCode,
		"user_code":        "ABCD-EFGH",
		"verification_uri": "https://screenote.test/oauth/device",
		"expires_in":       600,
		"interval":         5,
	}, func(w http.ResponseWriter, r *http.Request, attempt int) {
		writeDeviceOAuthError(w, "access_denied", "authorization denied for "+deviceCode)
	})
	defer server.Close()

	var stdout, stderr bytes.Buffer
	a := &app{
		stdout:     &stdout,
		stderr:     &stderr,
		httpClient: server.Client(),
		configPath: filepath.Join(t.TempDir(), "config.toml"),
		now:        clock.Now,
		wait:       clock.Wait,
	}
	a.flags.BaseURL = server.URL
	cmd := a.loginCommand()
	cmd.SetArgs([]string{"--device"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected terminal OAuth error")
	}
	if exit := writeError(&stderr, err); exit != ExitAuth {
		t.Fatalf("exit=%d err=%v", exit, err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if strings.Contains(stderr.String(), deviceCode) || strings.Contains(stderr.String(), url.QueryEscape(deviceCode)) {
		t.Fatalf("stderr leaked device code: %q", stderr.String())
	}

	decoder := json.NewDecoder(&stderr)
	var event deviceAuthorizationEvent
	if err := decoder.Decode(&event); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	var failure map[string]string
	if err := decoder.Decode(&failure); err != nil {
		t.Fatalf("decode failure: %v", err)
	}
	if event.Event != "device_authorization" || failure["code"] != "access_denied" || failure["error"] != "OAuth device authorization failed" {
		t.Fatalf("event=%#v failure=%#v", event, failure)
	}
}

func TestRunDeviceLoginMapsInitiationRateLimitToRateLimitExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                        serverURL(r),
				"authorization_endpoint":        serverURL(r) + "/oauth/authorize",
				"token_endpoint":                serverURL(r) + "/oauth/token",
				"registration_endpoint":         serverURL(r) + "/oauth/register",
				"device_authorization_endpoint": serverURL(r) + "/oauth/authorize_device",
				"grant_types_supported":         []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
			})
		case "/oauth/register":
			_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "device-client"})
		case "/oauth/authorize_device":
			writeDeviceOAuthStatusError(w, http.StatusTooManyRequests, "temporarily_unavailable", "try again later")
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	a := &app{stderr: &bytes.Buffer{}, httpClient: server.Client()}
	_, err := a.runDeviceLogin(context.Background(), server.URL)
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.Code != "temporarily_unavailable" || cliErr.Exit != ExitRateLimited {
		t.Fatalf("err=%T %#v", err, err)
	}
}

func TestRunDeviceLoginMapsPollingRateLimitToRateLimitExit(t *testing.T) {
	clock := newDeviceTestClock()
	server := newDeviceLoginServer(t, map[string]any{
		"device_code":      "device-secret",
		"user_code":        "ABCD-EFGH",
		"verification_uri": "https://screenote.test/oauth/device",
		"expires_in":       600,
		"interval":         5,
	}, func(w http.ResponseWriter, r *http.Request, attempt int) {
		writeDeviceOAuthStatusError(w, http.StatusTooManyRequests, "temporarily_unavailable", "try again later")
	})
	defer server.Close()

	a := &app{stderr: &bytes.Buffer{}, httpClient: server.Client(), now: clock.Now, wait: clock.Wait}
	_, err := a.runDeviceLogin(context.Background(), server.URL)
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.Code != "temporarily_unavailable" || cliErr.Exit != ExitRateLimited {
		t.Fatalf("err=%T %#v", err, err)
	}
	if !slices.Equal(clock.waits, []time.Duration{5 * time.Second}) {
		t.Fatalf("waits=%v", clock.waits)
	}
}

func TestDeviceLoginErrorUsesStableHTTPAndProtocolExitClasses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		code       string
		exit       int
	}{
		{name: "not found", statusCode: http.StatusNotFound, code: "not_found", exit: ExitNotFound},
		{name: "rate limited", statusCode: http.StatusTooManyRequests, code: "temporarily_unavailable", exit: ExitRateLimited},
		{name: "server error", statusCode: http.StatusServiceUnavailable, code: "temporarily_unavailable", exit: ExitGeneric},
		{name: "device denied", statusCode: http.StatusBadRequest, code: "access_denied", exit: ExitAuth},
		{name: "device expired", statusCode: http.StatusBadRequest, code: "expired_token", exit: ExitAuth},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := deviceLoginError(&screenote.OAuthError{
				StatusCode:  test.statusCode,
				Code:        test.code,
				Description: "device login failed",
			})
			var cliErr *cliError
			if !errors.As(err, &cliErr) || cliErr.Code != test.code || cliErr.Exit != test.exit {
				t.Fatalf("err=%T %#v", err, err)
			}
		})
	}
}

func TestRunDeviceLoginStopsLocallyAtExpiry(t *testing.T) {
	clock := newDeviceTestClock()
	attempts := 0
	server := newDeviceLoginServer(t, map[string]any{
		"device_code":      "device-secret",
		"user_code":        "ABCD-EFGH",
		"verification_uri": "https://screenote.test/oauth/device",
		"expires_in":       8,
		"interval":         5,
	}, func(w http.ResponseWriter, r *http.Request, attempt int) {
		attempts++
		writeDeviceOAuthError(w, "authorization_pending", "not approved yet")
	})
	defer server.Close()

	a := &app{stderr: &bytes.Buffer{}, httpClient: server.Client(), now: clock.Now, wait: clock.Wait}
	_, err := a.runDeviceLogin(context.Background(), server.URL)
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.Code != "expired_token" {
		t.Fatalf("err=%T %#v", err, err)
	}
	if attempts != 1 || !slices.Equal(clock.waits, []time.Duration{5 * time.Second, 3 * time.Second}) {
		t.Fatalf("attempts=%d waits=%v", attempts, clock.waits)
	}
}

func TestRunDeviceLoginBoundsInFlightPollByDeviceExpiry(t *testing.T) {
	clock := newDeviceTestClock()
	pollStarted := make(chan struct{}, 1)
	server := newDeviceLoginServer(t, map[string]any{
		"device_code":      "device-secret",
		"user_code":        "ABCD-EFGH",
		"verification_uri": "https://screenote.test/oauth/device",
		"expires_in":       2,
		"interval":         1,
	}, func(w http.ResponseWriter, r *http.Request, attempt int) {
		pollStarted <- struct{}{}
		<-r.Context().Done()
	})
	defer server.Close()

	a := &app{
		stderr:     &bytes.Buffer{},
		httpClient: server.Client(),
		now:        clock.Now,
		wait: func(ctx context.Context, delay time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			clock.waits = append(clock.waits, delay)
			clock.now = clock.start.Add(1950 * time.Millisecond)
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := a.runDeviceLogin(ctx, server.URL)
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.Code != "expired_token" || cliErr.Exit != ExitAuth {
		t.Fatalf("err=%T %#v", err, err)
	}
	select {
	case <-pollStarted:
	default:
		t.Fatal("expected one token poll")
	}
	if !slices.Equal(clock.waits, []time.Duration{time.Second}) {
		t.Fatalf("waits=%v", clock.waits)
	}
}

func TestRunDeviceLoginPreservesParentCancellationDuringPoll(t *testing.T) {
	clock := newDeviceTestClock()
	ctx, cancel := context.WithCancel(context.Background())
	server := newDeviceLoginServer(t, map[string]any{
		"device_code":      "device-secret",
		"user_code":        "ABCD-EFGH",
		"verification_uri": "https://screenote.test/oauth/device",
		"expires_in":       600,
		"interval":         5,
	}, func(w http.ResponseWriter, r *http.Request, attempt int) {
		cancel()
		<-r.Context().Done()
	})
	defer server.Close()

	a := &app{stderr: &bytes.Buffer{}, httpClient: server.Client(), now: clock.Now, wait: clock.Wait}
	_, err := a.runDeviceLogin(ctx, server.URL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%T %#v", err, err)
	}
}

func TestRunDeviceLoginHonorsContextCancellationBeforeFirstPoll(t *testing.T) {
	server := newDeviceLoginServer(t, map[string]any{
		"device_code":      "device-secret",
		"user_code":        "ABCD-EFGH",
		"verification_uri": "https://screenote.test/oauth/device",
		"expires_in":       600,
		"interval":         5,
	}, func(w http.ResponseWriter, r *http.Request, attempt int) {
		t.Fatal("canceled device login must not poll")
	})
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	a := &app{
		stderr:     &bytes.Buffer{},
		httpClient: server.Client(),
		now:        time.Now,
		wait: func(context.Context, time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	_, err := a.runDeviceLogin(ctx, server.URL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestRunDeviceLoginFailsFastWhenMetadataDoesNotAdvertiseDeviceGrant(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 serverURL(r),
			"authorization_endpoint": serverURL(r) + "/oauth/authorize",
			"token_endpoint":         serverURL(r) + "/oauth/token",
			"registration_endpoint":  serverURL(r) + "/oauth/register",
			"grant_types_supported":  []string{"authorization_code", "refresh_token"},
		})
	}))
	defer server.Close()

	a := &app{stderr: &bytes.Buffer{}, httpClient: server.Client()}
	_, err := a.runDeviceLogin(context.Background(), server.URL)
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.Code != "device_authorization_unsupported" || cliErr.Exit != ExitUsage {
		t.Fatalf("err=%T %#v", err, err)
	}
	if requests != 1 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestSaveLoginCredentialsSwitchesConfiguredBaseURLAndClearsLegacyToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := appconfig.Save(configPath, appconfig.Values{Token: "legacy-token", BaseURL: "https://old.example"}); err != nil {
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
	if values.Token != "" || values.BaseURL != "https://new.example" || values.Login == nil || values.Login.AccessToken != "access-1" {
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

func TestHiddenConfigSetTokenFlagRemainsCompatible(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	_, stderr, code := runCLI(t, []string{"--config", configPath, "config", "set", "--token", "explicit"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	values, err := appconfig.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if values.Token != "explicit" {
		t.Fatalf("token=%q", values.Token)
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

type deviceTestClock struct {
	start time.Time
	now   time.Time
	waits []time.Duration
}

func newDeviceTestClock() *deviceTestClock {
	start := time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC)
	return &deviceTestClock{start: start, now: start}
}

func (c *deviceTestClock) Now() time.Time {
	return c.now
}

func (c *deviceTestClock) Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.waits = append(c.waits, delay)
	c.now = c.now.Add(delay)
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newDeviceLoginServer(t *testing.T, deviceResponse map[string]any, tokenHandler func(http.ResponseWriter, *http.Request, int)) *httptest.Server {
	t.Helper()
	tokenAttempts := 0
	expectedDeviceCode, _ := deviceResponse["device_code"].(string)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                        serverURL(r),
				"authorization_endpoint":        serverURL(r) + "/oauth/authorize",
				"token_endpoint":                serverURL(r) + "/oauth/token",
				"registration_endpoint":         serverURL(r) + "/oauth/register",
				"device_authorization_endpoint": serverURL(r) + "/oauth/authorize_device",
				"grant_types_supported":         []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
			})
		case "/oauth/register":
			var payload struct {
				RedirectURIs            []string `json:"redirect_uris"`
				GrantTypes              []string `json:"grant_types"`
				TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(payload.RedirectURIs, []string{"http://127.0.0.1/callback"}) ||
				!slices.Equal(payload.GrantTypes, []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"}) ||
				payload.TokenEndpointAuthMethod != "none" {
				t.Fatalf("registration payload=%#v", payload)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "device-client"})
		case "/oauth/authorize_device":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("client_id") != "device-client" || r.Form.Get("scope") != oauthScope {
				t.Fatalf("device authorization form=%v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(deviceResponse)
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("client_id") != "device-client" || r.Form.Get("device_code") != expectedDeviceCode {
				t.Fatalf("token form=%v", r.Form)
			}
			tokenAttempts++
			tokenHandler(w, r, tokenAttempts)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
}

func writeDeviceOAuthError(w http.ResponseWriter, code, description string) {
	writeDeviceOAuthStatusError(w, http.StatusBadRequest, code, description)
}

func writeDeviceOAuthStatusError(w http.ResponseWriter, status int, code, description string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": description,
	})
}

func serverURL(r *http.Request) string {
	u := url.URL{Scheme: "http", Host: r.Host}
	return u.String()
}
