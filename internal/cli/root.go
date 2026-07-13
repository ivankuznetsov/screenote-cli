package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	appconfig "github.com/ivankuznetsov/screenote-cli/internal/config"
	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
	"github.com/spf13/cobra"
)

type app struct {
	stdin                io.Reader
	stdout               io.Writer
	stderr               io.Writer
	httpClient           *http.Client
	snapshotPollInterval time.Duration
	snapshotPollJitter   func(time.Duration) time.Duration
	flags                appconfig.Values
	configPath           string
	now                  func() time.Time
	wait                 func(context.Context, time.Duration) error
}

func Execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{stdin: stdin, stdout: stdout, stderr: stderr}
	cmd := a.rootCommand(ctx)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		return writeError(stderr, err)
	}
	return ExitOK
}

func NewTestCommand(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, httpClient *http.Client) *cobra.Command {
	a := &app{
		stdin: stdin, stdout: stdout, stderr: stderr, httpClient: httpClient,
		snapshotPollInterval: 5 * time.Millisecond,
		snapshotPollJitter:   func(interval time.Duration) time.Duration { return interval },
	}
	return a.rootCommand(ctx)
}

func (a *app) rootCommand(_ context.Context) *cobra.Command {
	root := &cobra.Command{
		Use:           "screenote",
		Short:         "Screenote REST CLI",
		Args:          rejectArgs,
		RunE:          showHelp,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageError("invalid_flag", err.Error())
	})
	root.PersistentFlags().StringVar(&a.flags.Token, "token", "", "Screenote OAuth bearer token")
	_ = root.PersistentFlags().MarkHidden("token")
	root.PersistentFlags().StringVar(&a.flags.BaseURL, "base-url", "", "Screenote base URL")
	root.PersistentFlags().StringVar(&a.flags.Project, "project", "", "Screenote project ID")
	root.PersistentFlags().StringVar(&a.configPath, "config", "", "Config file path")

	root.AddCommand(
		a.configCommand(),
		a.loginCommand(),
		a.logoutCommand(),
		a.projectCommand(),
		a.pageCommand(),
		a.screenshotCommand(),
		a.snapshotCommand(),
		a.annotationCommand(),
		a.commentCommand(),
	)
	return root
}

func (a *app) resolvedConfig() (appconfig.Resolved, error) {
	return appconfig.Resolve(appconfig.Options{
		ConfigPath: a.configPath,
		Flags:      a.flags,
	})
}

func (a *app) client(ctx context.Context) (*screenote.Client, appconfig.Resolved, error) {
	resolved, err := a.resolvedConfig()
	if err != nil {
		return nil, resolved, err
	}
	if resolved.BaseURL == "" {
		return nil, resolved, usageError("missing_base_url", "base URL is required; set --base-url, SCREENOTE_BASE_URL, or config base_url")
	}
	if resolved.Token == "" {
		token, err := a.storedLoginToken(ctx, resolved)
		if err != nil {
			return nil, resolved, err
		}
		resolved.Token = token
	}

	client, err := screenote.NewClient(resolved.BaseURL, resolved.Token, a.httpClient)
	if err != nil {
		return nil, resolved, usageError("invalid_base_url", err.Error())
	}
	return client, resolved, nil
}

// clientForProject resolves the required project locally before constructing a
// client, so a missing project fails with the local usage error without first
// triggering OAuth discovery/refresh network calls or mutating stored config.
func (a *app) clientForProject(ctx context.Context) (*screenote.Client, string, error) {
	resolved, err := a.resolvedConfig()
	if err != nil {
		return nil, "", err
	}
	project, err := a.projectID(resolved)
	if err != nil {
		return nil, "", err
	}
	client, _, err := a.client(ctx)
	if err != nil {
		return nil, "", err
	}
	return client, project, nil
}

func (a *app) storedLoginToken(ctx context.Context, resolved appconfig.Resolved) (string, error) {
	path := defaultConfigPath(a.configPath)
	values, err := appconfig.LoadExpanded(path)
	if err != nil {
		return "", err
	}
	credentials := values.Login
	if credentials == nil || credentials.AccessToken == "" {
		return "", usageError("missing_token", "OAuth login is required; run screenote login or screenote login --device")
	}
	if credentials.BaseURL != "" && resolved.BaseURL != "" && !sameBaseURL(credentials.BaseURL, resolved.BaseURL) {
		return "", authError("invalid_token", "stored login credentials are for a different base URL")
	}
	if credentials.ExpiresAt.IsZero() || time.Until(credentials.ExpiresAt) > time.Minute {
		return credentials.AccessToken, nil
	}
	if credentials.RefreshToken == "" {
		return "", authError("invalid_token", "stored OAuth token is expired and has no refresh token")
	}

	metadata, err := screenote.DiscoverOAuth(ctx, resolved.BaseURL, a.httpClient)
	if err != nil {
		return "", authError("invalid_token", "stored OAuth token refresh failed: "+err.Error())
	}
	if credentials.Issuer != "" && metadata.Issuer != credentials.Issuer {
		return "", authError("invalid_token", "stored OAuth issuer does not match discovered issuer")
	}
	response, err := screenote.RefreshAccessToken(ctx, metadata, credentials.ClientID, credentials.RefreshToken, a.httpClient)
	if err != nil {
		return "", authError("invalid_token", "stored OAuth token refresh failed: "+err.Error())
	}
	credentials.AccessToken = response.AccessToken
	if response.RefreshToken != "" {
		credentials.RefreshToken = response.RefreshToken
	}
	credentials.ExpiresAt = screenote.ExpiresAt(response, time.Now())
	values.Login = credentials
	if err := appconfig.Save(path, values); err != nil {
		return "", err
	}
	return credentials.AccessToken, nil
}

// sameBaseURL compares two base URLs ignoring trailing-slash differences so a
// stored "https://x" and a resolved "https://x/" don't force an unnecessary
// re-login. It fails closed: unparseable inputs fall back to trimmed strings.
func sameBaseURL(a, b string) bool {
	return normalizeBaseURL(a) == normalizeBaseURL(b)
}

func normalizeBaseURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return strings.TrimRight(raw, "/")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}

func (a *app) projectID(resolved appconfig.Resolved) (string, error) {
	if resolved.Project != "" {
		return resolved.Project, nil
	}
	return "", usageError("missing_project", "project is required; set --project, SCREENOTE_PROJECT, or config project")
}

func writeJSON(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
}

func writeRawJSON(w io.Writer, raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	_, err := w.Write(raw)
	if err != nil {
		return err
	}
	if raw[len(raw)-1] != '\n' {
		_, err = io.WriteString(w, "\n")
	}
	return err
}

func intString(id int) string {
	return strconv.Itoa(id)
}

func defaultConfigPath(path string) string {
	if path != "" {
		return path
	}
	return appconfig.DefaultConfigPath
}
