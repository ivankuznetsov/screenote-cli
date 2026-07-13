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
	"slices"
	"strings"
	"time"
)

const (
	DeviceCodeGrantType                = "urn:ietf:params:oauth:grant-type:device_code"
	DeviceOAuthRedirectURI             = "http://127.0.0.1/callback"
	DefaultDevicePollIntervalSeconds   = 5
	deviceOAuthTokenEndpointAuthMethod = "none"
)

type OAuthMetadata struct {
	Issuer                      string   `json:"issuer"`
	AuthorizationEndpoint       string   `json:"authorization_endpoint"`
	TokenEndpoint               string   `json:"token_endpoint"`
	RegistrationEndpoint        string   `json:"registration_endpoint"`
	DeviceAuthorizationEndpoint string   `json:"device_authorization_endpoint"`
	ScopesSupported             []string `json:"scopes_supported"`
	CodeChallengeMethods        []string `json:"code_challenge_methods_supported"`
	GrantTypesSupported         []string `json:"grant_types_supported"`
}

type OAuthRegistration struct {
	ClientID string `json:"client_id"`
}

type DeviceAuthorizationResponse struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
	Interval                int
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// OAuthError preserves the protocol error fields returned by OAuth endpoints.
// Device authorization polling depends on the exact error code to distinguish
// retryable responses from terminal failures.
type OAuthError struct {
	StatusCode  int
	Code        string
	Description string
	URI         string
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return e.Description
	}
	if e.Code != "" {
		return e.Code
	}
	return http.StatusText(e.StatusCode)
}

func (m OAuthMetadata) SupportsDeviceAuthorization() bool {
	return m.DeviceAuthorizationEndpoint != "" && slices.Contains(m.GrantTypesSupported, DeviceCodeGrantType)
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
	body := map[string]any{
		"client_name":   "Screenote CLI",
		"redirect_uris": []string{redirectURI},
	}
	return registerOAuthClient(ctx, metadata, body, httpClient)
}

func RegisterDeviceOAuthClient(ctx context.Context, metadata OAuthMetadata, httpClient *http.Client) (OAuthRegistration, error) {
	body := map[string]any{
		"client_name":                "Screenote CLI",
		"redirect_uris":              []string{DeviceOAuthRedirectURI},
		"grant_types":                []string{DeviceCodeGrantType, "refresh_token"},
		"token_endpoint_auth_method": deviceOAuthTokenEndpointAuthMethod,
	}
	return registerOAuthClient(ctx, metadata, body, httpClient)
}

func registerOAuthClient(ctx context.Context, metadata OAuthMetadata, body map[string]any, httpClient *http.Client) (OAuthRegistration, error) {
	var registration OAuthRegistration
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

func RequestDeviceAuthorization(ctx context.Context, metadata OAuthMetadata, clientID, scope string, httpClient *http.Client) (DeviceAuthorizationResponse, error) {
	var response DeviceAuthorizationResponse
	if metadata.DeviceAuthorizationEndpoint == "" {
		return response, errors.New("OAuth metadata is missing the device authorization endpoint")
	}
	form := url.Values{
		"client_id": []string{clientID},
		"scope":     []string{scope},
	}
	raw, status, err := postOAuthForm(ctx, metadata.DeviceAuthorizationEndpoint, form, httpClient)
	if err != nil {
		return response, err
	}
	if status < 200 || status > 299 {
		return response, parseOAuthError(status, raw)
	}

	var payload struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                *int   `json:"interval"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return response, err
	}
	if payload.DeviceCode == "" {
		return response, errors.New("device authorization endpoint did not return a device_code")
	}
	if payload.UserCode == "" {
		return response, errors.New("device authorization endpoint did not return a user_code")
	}
	if payload.VerificationURI == "" {
		return response, errors.New("device authorization endpoint did not return a verification_uri")
	}
	if payload.ExpiresIn <= 0 {
		return response, errors.New("device authorization endpoint returned an invalid expires_in")
	}
	interval := DefaultDevicePollIntervalSeconds
	if payload.Interval != nil {
		if *payload.Interval <= 0 {
			return response, errors.New("device authorization endpoint returned an invalid interval")
		}
		interval = *payload.Interval
	}

	return DeviceAuthorizationResponse{
		DeviceCode:              payload.DeviceCode,
		UserCode:                payload.UserCode,
		VerificationURI:         payload.VerificationURI,
		VerificationURIComplete: payload.VerificationURIComplete,
		ExpiresIn:               payload.ExpiresIn,
		Interval:                interval,
	}, nil
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

func ExchangeDeviceCode(ctx context.Context, metadata OAuthMetadata, clientID, deviceCode string, httpClient *http.Client) (TokenResponse, error) {
	form := url.Values{
		"grant_type":  []string{DeviceCodeGrantType},
		"device_code": []string{deviceCode},
		"client_id":   []string{clientID},
	}
	return oauthTokenRequest(ctx, metadata.TokenEndpoint, form, httpClient)
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
	return tokenRequestWithErrorParser(ctx, endpoint, form, httpClient, parseError)
}

func oauthTokenRequest(ctx context.Context, endpoint string, form url.Values, httpClient *http.Client) (TokenResponse, error) {
	return tokenRequestWithErrorParser(ctx, endpoint, form, httpClient, parseOAuthError)
}

func tokenRequestWithErrorParser(ctx context.Context, endpoint string, form url.Values, httpClient *http.Client, parseResponseError func(int, []byte) error) (TokenResponse, error) {
	var token TokenResponse
	raw, status, err := postOAuthForm(ctx, endpoint, form, httpClient)
	if err != nil {
		return token, err
	}
	if status < 200 || status > 299 {
		return token, parseResponseError(status, raw)
	}
	if err := json.Unmarshal(raw, &token); err != nil {
		return token, err
	}
	if token.AccessToken == "" {
		return token, fmt.Errorf("token endpoint did not return an access_token")
	}
	return token, nil
}

func postOAuthForm(ctx context.Context, endpoint string, form url.Values, httpClient *http.Client) ([]byte, int, error) {
	httpClient = httpClientOrDefault(httpClient)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return raw, resp.StatusCode, nil
}

func parseOAuthError(status int, raw []byte) error {
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		ErrorURI         string `json:"error_uri"`
	}
	_ = json.Unmarshal(raw, &payload)
	if payload.Error == "" {
		payload.Error = statusCode(status)
	}
	if payload.ErrorDescription == "" {
		payload.ErrorDescription = http.StatusText(status)
	}
	return &OAuthError{
		StatusCode:  status,
		Code:        payload.Error,
		Description: payload.ErrorDescription,
		URI:         payload.ErrorURI,
	}
}
