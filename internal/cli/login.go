package cli

import (
	"context"
	"net"
	"net/http"
	"os/exec"
	"runtime"
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

func (a *app) loginCommand() *cobra.Command {
	return &cobra.Command{
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
			credentials, err := a.runLogin(cmd.Context(), resolved.BaseURL)
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
}

func (a *app) saveLoginCredentials(baseURL string, credentials *appconfig.LoginCredentials) (string, error) {
	path := defaultConfigPath(a.configPath)
	values, err := appconfig.LoadExpanded(path)
	if err != nil {
		return "", err
	}
	values.Login = credentials
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
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
		return &appconfig.LoginCredentials{
			AccessToken:  response.AccessToken,
			RefreshToken: response.RefreshToken,
			ExpiresAt:    screenote.ExpiresAt(response, time.Now()),
			ClientID:     registration.ClientID,
			BaseURL:      baseURL,
			Issuer:       metadata.Issuer,
		}, nil
	}
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
