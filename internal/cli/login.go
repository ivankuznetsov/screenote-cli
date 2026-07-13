package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	appconfig "github.com/ivankuznetsov/screenote-cli/internal/config"
	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
	"github.com/spf13/cobra"
)

const oauthScope = "mcp_read mcp_write"

var openBrowser = func(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}

var listenLoopback = func() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

func (a *app) loginCommand() *cobra.Command {
	var device bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authorize the CLI with OAuth",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := a.resolvedConfig()
			if err != nil {
				return err
			}
			if resolved.BaseURL == "" {
				return usageError("missing_base_url", "base URL is required; set --base-url, SCREENOTE_BASE_URL, or config base_url")
			}
			var credentials *appconfig.LoginCredentials
			if device {
				credentials, err = a.runDeviceLogin(cmd.Context(), resolved.BaseURL)
			} else {
				credentials, err = a.runLogin(cmd.Context(), resolved.BaseURL)
			}
			if err != nil {
				return err
			}
			path, err := a.saveLoginCredentials(resolved.BaseURL, credentials)
			if err != nil {
				return err
			}
			return writeJSON(a.stdout, map[string]any{"ok": true, "path": path})
		},
	}
	cmd.Flags().BoolVar(&device, "device", false, "Authorize on another device (for SSH and headless sessions)")
	return cmd
}

func (a *app) saveLoginCredentials(baseURL string, credentials *appconfig.LoginCredentials) (string, error) {
	path := defaultConfigPath(a.configPath)
	values, err := appconfig.LoadExpanded(path)
	if err != nil {
		return "", err
	}
	values.Login = credentials
	// A successful OAuth login replaces any legacy file-level bearer token so
	// subsequent commands use the refreshable credentials that were just saved.
	values.Token = ""
	// There is one stored login credential set, so keep the configured server
	// aligned with it even when login used a flag/env override.
	values.BaseURL = baseURL
	if err := appconfig.Save(path, values); err != nil {
		return "", err
	}
	return path, nil
}

func (a *app) logoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove stored OAuth login credentials",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := defaultConfigPath(a.configPath)
			values, err := appconfig.LoadExpanded(path)
			if err != nil {
				return err
			}
			values.Login = nil
			if err := appconfig.Save(path, values); err != nil {
				return err
			}
			return writeJSON(a.stdout, map[string]any{"ok": true, "path": path})
		},
	}
}

func (a *app) runLogin(ctx context.Context, baseURL string) (*appconfig.LoginCredentials, error) {
	metadata, err := screenote.DiscoverOAuth(ctx, baseURL, a.httpClient)
	if err != nil {
		return nil, err
	}
	listener, err := listenLoopback()
	if err != nil {
		return nil, err
	}
	defer listener.Close()

	redirectURI := "http://" + listener.Addr().String() + "/callback"
	registration, err := screenote.RegisterOAuthClient(ctx, metadata, redirectURI, a.httpClient)
	if err != nil {
		return nil, err
	}
	verifier, err := screenote.RandomToken(32)
	if err != nil {
		return nil, err
	}
	state, err := screenote.RandomToken(24)
	if err != nil {
		return nil, err
	}
	authURL, err := screenote.AuthorizationURL(metadata, registration.ClientID, redirectURI, verifier, state, oauthScope)
	if err != nil {
		return nil, err
	}

	result := make(chan callbackResult, 1)
	server := &http.Server{Handler: callbackHandler(state, result)}
	go func() {
		_ = server.Serve(listener)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := openBrowser(authURL); err != nil {
		if writeErr := writeJSON(a.stderr, map[string]string{
			"code":              "browser_open_failed",
			"message":           "browser could not be opened; open authorization_url to continue login",
			"authorization_url": authURL,
		}); writeErr != nil {
			return nil, writeErr
		}
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case received := <-result:
		if received.err != nil {
			return nil, received.err
		}
		response, err := screenote.ExchangeCode(ctx, metadata, registration.ClientID, redirectURI, received.code, verifier, a.httpClient)
		if err != nil {
			return nil, err
		}
		return newLoginCredentials(baseURL, metadata, registration, response, time.Now()), nil
	}
}

type deviceAuthorizationEvent struct {
	Event            string `json:"event"`
	AuthorizationURL string `json:"authorization_url"`
	VerificationURI  string `json:"verification_uri"`
	UserCode         string `json:"user_code"`
	ExpiresIn        int    `json:"expires_in"`
	Interval         int    `json:"interval"`
}

func (a *app) runDeviceLogin(ctx context.Context, baseURL string) (*appconfig.LoginCredentials, error) {
	metadata, err := screenote.DiscoverOAuth(ctx, baseURL, a.httpClient)
	if err != nil {
		return nil, err
	}
	if !metadata.SupportsDeviceAuthorization() {
		return nil, usageError(
			"device_authorization_unsupported",
			"Screenote server does not advertise OAuth device authorization",
		)
	}

	registration, err := screenote.RegisterDeviceOAuthClient(ctx, metadata, a.httpClient)
	if err != nil {
		return nil, err
	}
	requestedAt := a.currentTime()
	deviceAuthorization, err := screenote.RequestDeviceAuthorization(ctx, metadata, registration.ClientID, oauthScope, a.httpClient)
	if err != nil {
		return nil, deviceLoginError(err)
	}

	event, err := safeDeviceAuthorizationEvent(deviceAuthorization)
	if err != nil {
		return nil, err
	}
	if err := writeJSON(a.stderr, event); err != nil {
		return nil, err
	}

	expiresAt := requestedAt.Add(time.Duration(deviceAuthorization.ExpiresIn) * time.Second)
	pollInterval := time.Duration(deviceAuthorization.Interval) * time.Second
	nextDelay := pollInterval

	for {
		if err := a.waitForDevicePoll(ctx, nextDelay, expiresAt); err != nil {
			return nil, err
		}

		remaining := expiresAt.Sub(a.currentTime())
		if remaining <= 0 {
			return nil, deviceAuthorizationExpiredError()
		}
		pollCtx, cancelPoll := context.WithTimeout(ctx, remaining)
		response, err := screenote.ExchangeDeviceCode(pollCtx, metadata, registration.ClientID, deviceAuthorization.DeviceCode, a.httpClient)
		pollCtxErr := pollCtx.Err()
		cancelPoll()
		if err == nil {
			return newLoginCredentials(baseURL, metadata, registration, response, a.currentTime()), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(pollCtxErr, context.DeadlineExceeded) {
			return nil, deviceAuthorizationExpiredError()
		}

		var oauthError *screenote.OAuthError
		if errors.As(err, &oauthError) {
			switch oauthError.Code {
			case "authorization_pending":
				nextDelay = pollInterval
				continue
			case "slow_down":
				pollInterval += 5 * time.Second
				nextDelay = pollInterval
				continue
			default:
				return nil, deviceLoginError(oauthError, deviceAuthorization.DeviceCode)
			}
		}

		if isTransportTimeout(err) {
			nextDelay *= 2
			continue
		}
		return nil, redactDeviceError(err, deviceAuthorization.DeviceCode)
	}
}

func newLoginCredentials(
	baseURL string,
	metadata screenote.OAuthMetadata,
	registration screenote.OAuthRegistration,
	response screenote.TokenResponse,
	now time.Time,
) *appconfig.LoginCredentials {
	return &appconfig.LoginCredentials{
		AccessToken:  response.AccessToken,
		RefreshToken: response.RefreshToken,
		ExpiresAt:    screenote.ExpiresAt(response, now),
		ClientID:     registration.ClientID,
		BaseURL:      baseURL,
		Issuer:       metadata.Issuer,
	}
}

func safeDeviceAuthorizationEvent(response screenote.DeviceAuthorizationResponse) (deviceAuthorizationEvent, error) {
	if containsDeviceCode(response.VerificationURI, response.DeviceCode) ||
		containsDeviceCode(response.UserCode, response.DeviceCode) {
		return deviceAuthorizationEvent{}, authError(
			"invalid_device_authorization_response",
			"OAuth device authorization response contained unsafe verification details",
		)
	}

	authorizationURL := response.VerificationURIComplete
	if authorizationURL == "" || containsDeviceCode(authorizationURL, response.DeviceCode) {
		authorizationURL = response.VerificationURI
	}
	return deviceAuthorizationEvent{
		Event:            "device_authorization",
		AuthorizationURL: authorizationURL,
		VerificationURI:  response.VerificationURI,
		UserCode:         response.UserCode,
		ExpiresIn:        response.ExpiresIn,
		Interval:         response.Interval,
	}, nil
}

func (a *app) currentTime() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *app) waitForDevicePoll(ctx context.Context, delay time.Duration, expiresAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	remaining := expiresAt.Sub(a.currentTime())
	if remaining <= 0 {
		return deviceAuthorizationExpiredError()
	}
	if delay >= remaining {
		if err := a.waitFor(ctx, remaining); err != nil {
			return err
		}
		return deviceAuthorizationExpiredError()
	}
	if err := a.waitFor(ctx, delay); err != nil {
		return err
	}
	if !a.currentTime().Before(expiresAt) {
		return deviceAuthorizationExpiredError()
	}
	return nil
}

func deviceAuthorizationExpiredError() error {
	return authError("expired_token", "OAuth device authorization expired before login completed")
}

func (a *app) waitFor(ctx context.Context, delay time.Duration) error {
	if a.wait != nil {
		return a.wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func deviceLoginError(err error, deviceCodes ...string) error {
	var oauthError *screenote.OAuthError
	if !errors.As(err, &oauthError) {
		return redactDeviceError(err, deviceCodes...)
	}
	message := oauthError.Description
	if message == "" {
		message = oauthError.Code
	}
	exit := exitForHTTP(oauthError.StatusCode)
	switch oauthError.Code {
	case "access_denied", "expired_token", "invalid_client", "invalid_grant", "unauthorized_client":
		exit = ExitAuth
	}
	code := oauthError.Code
	for _, deviceCode := range deviceCodes {
		if containsDeviceCode(code, deviceCode) {
			code = "oauth_error"
		}
		if containsDeviceCode(message, deviceCode) {
			message = "OAuth device authorization failed"
		}
	}
	return &cliError{Code: code, Message: message, Exit: exit}
}

func redactDeviceError(err error, deviceCodes ...string) error {
	if err == nil {
		return nil
	}
	for _, deviceCode := range deviceCodes {
		if containsDeviceCode(err.Error(), deviceCode) {
			return errors.New("OAuth device authorization failed")
		}
	}
	return err
}

func containsDeviceCode(value, deviceCode string) bool {
	if value == "" || deviceCode == "" {
		return false
	}
	if strings.Contains(value, deviceCode) {
		return true
	}
	for range 2 {
		decoded, err := url.QueryUnescape(value)
		if err != nil || decoded == value {
			break
		}
		if strings.Contains(decoded, deviceCode) {
			return true
		}
		value = decoded
	}
	return false
}

func isTransportTimeout(err error) bool {
	var netError net.Error
	return errors.As(err, &netError) && netError.Timeout()
}

type callbackResult struct {
	code string
	err  error
}

func callbackHandler(expectedState string, result chan<- callbackResult) http.Handler {
	// send never blocks: result is buffered for one value, so a second callback
	// after the first result is consumed is dropped rather than wedging the
	// handler (which would stall the deferred server.Shutdown indefinitely).
	send := func(r callbackResult) {
		select {
		case result <- r:
		default:
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query()
		if query.Get("state") != expectedState {
			http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
			send(callbackResult{err: usageError("invalid_oauth_state", "OAuth callback state did not match")})
			return
		}
		if errText := query.Get("error"); errText != "" {
			http.Error(w, errText, http.StatusBadRequest)
			send(callbackResult{err: authError("oauth_error", errText)})
			return
		}
		code := query.Get("code")
		if code == "" {
			http.Error(w, "Missing OAuth code", http.StatusBadRequest)
			send(callbackResult{err: usageError("missing_oauth_code", "OAuth callback did not include an authorization code")})
			return
		}
		// Write the success page before signaling so the deferred shutdown, which
		// runs once runLogin consumes this result, cannot race ahead of the write
		// and truncate the response.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Screenote login complete. You can close this window."))
		send(callbackResult{code: code})
	})
}
