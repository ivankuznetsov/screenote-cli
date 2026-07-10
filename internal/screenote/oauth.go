package screenote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type OAuthMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	GrantTypesSupported   []string `json:"grant_types_supported"`
}

type OAuthRegistration struct {
	ClientID string `json:"client_id"`
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

func DiscoverOAuth(ctx context.Context, baseURL string, httpClient *http.Client) (OAuthMetadata, error) {
	var metadata OAuthMetadata
	u, err := metadataURL(baseURL)
	if err != nil {
		return metadata, err
	}
	httpClient = httpClientOrDefault(httpClient)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return metadata, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return metadata, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(resp.Body)
		return metadata, parseError(resp.StatusCode, raw)
	}
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return metadata, err
	}
	if metadata.AuthorizationEndpoint == "" || metadata.TokenEndpoint == "" || metadata.RegistrationEndpoint == "" {
		return metadata, errors.New("OAuth metadata is missing required endpoints")
	}
	return metadata, nil
}

func RegisterOAuthClient(ctx context.Context, metadata OAuthMetadata, redirectURI string, httpClient *http.Client) (OAuthRegistration, error) {
	var registration OAuthRegistration
	body := map[string]any{
		"client_name":   "Screenote CLI",
		"redirect_uris": []string{redirectURI},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return registration, err
	}
	httpClient = httpClientOrDefault(httpClient)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, metadata.RegistrationEndpoint, bytes.NewReader(raw))
	if err != nil {
		return registration, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return registration, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(resp.Body)
		return registration, parseError(resp.StatusCode, raw)
	}
	if err := json.NewDecoder(resp.Body).Decode(&registration); err != nil {
		return registration, err
	}
	if registration.ClientID == "" {
		return registration, errors.New("OAuth registration did not return a client_id")
	}
	return registration, nil
}

func AuthorizationURL(metadata OAuthMetadata, clientID, redirectURI, verifier, state, scope string) (string, error) {
	u, err := url.Parse(metadata.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	values := u.Query()
	values.Set("response_type", "code")
	values.Set("client_id", clientID)
	values.Set("redirect_uri", redirectURI)
	values.Set("scope", scope)
	values.Set("state", state)
	values.Set("code_challenge", PKCEChallenge(verifier))
	values.Set("code_challenge_method", "S256")
	u.RawQuery = values.Encode()
	return u.String(), nil
}

func ExchangeCode(ctx context.Context, metadata OAuthMetadata, clientID, redirectURI, code, verifier string, httpClient *http.Client) (TokenResponse, error) {
	form := url.Values{
		"grant_type":    []string{"authorization_code"},
		"client_id":     []string{clientID},
		"redirect_uri":  []string{redirectURI},
		"code":          []string{code},
		"code_verifier": []string{verifier},
	}
	return tokenRequest(ctx, metadata.TokenEndpoint, form, httpClient)
}

func RefreshAccessToken(ctx context.Context, metadata OAuthMetadata, clientID, refreshToken string, httpClient *http.Client) (TokenResponse, error) {
	form := url.Values{
		"grant_type":    []string{"refresh_token"},
		"client_id":     []string{clientID},
		"refresh_token": []string{refreshToken},
	}
	return tokenRequest(ctx, metadata.TokenEndpoint, form, httpClient)
}

func RandomToken(bytesLen int) (string, error) {
	data := make([]byte, bytesLen)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func ExpiresAt(response TokenResponse, now time.Time) time.Time {
	if response.ExpiresIn <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(response.ExpiresIn) * time.Second)
}

func metadataURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("base url must include scheme and host")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/.well-known/oauth-authorization-server"
	parsed.RawQuery = ""
	return parsed.String(), nil
}

func tokenRequest(ctx context.Context, endpoint string, form url.Values, httpClient *http.Client) (TokenResponse, error) {
	var token TokenResponse
	httpClient = httpClientOrDefault(httpClient)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return token, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return token, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return token, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return token, parseError(resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &token); err != nil {
		return token, err
	}
	if token.AccessToken == "" {
		return token, fmt.Errorf("token endpoint did not return an access_token")
	}
	return token, nil
}
