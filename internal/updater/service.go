package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultAPIURL            = "https://api.github.com/repos/ivankuznetsov/screenote-cli/releases/latest"
	defaultCacheTTL          = 24 * time.Hour
	defaultRetryTTL          = 24 * time.Hour
	defaultNotificationTTL   = 24 * time.Hour
	maxReleaseMetadataBytes  = 1 << 20
	updateCacheSchemaVersion = 1
	updateCheckUserAgent     = "screenote-cli-update-check"
)

var ErrDevelopmentBuild = errors.New("development builds cannot be updated automatically")

type CheckResult struct {
	CurrentVersion  string
	LatestVersion   string
	UpdateAvailable bool
	Known           bool
	Channel         string
}

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type release struct {
	Version string
	Assets  []releaseAsset
}

type releasePayload struct {
	TagName    string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type cacheState struct {
	SchemaVersion   int       `json:"schema_version"`
	LatestVersion   string    `json:"latest_version,omitempty"`
	ETag            string    `json:"etag,omitempty"`
	CheckedAt       time.Time `json:"checked_at,omitempty"`
	AttemptedAt     time.Time `json:"attempted_at,omitempty"`
	NotifiedVersion string    `json:"notified_version,omitempty"`
	NotifiedAt      time.Time `json:"notified_at,omitempty"`
}

type Service struct {
	HTTPClient      *http.Client
	APIURL          string
	CachePath       string
	Now             func() time.Time
	CacheTTL        time.Duration
	RetryTTL        time.Duration
	NotificationTTL time.Duration
	Executable      func() (string, error)
	RunCommand      CommandRunner
}

func NewService(httpClient *http.Client) *Service {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Minute}
	}
	cachePath := ""
	if cacheDir, err := os.UserCacheDir(); err == nil {
		cachePath = filepath.Join(cacheDir, "screenote", "update-check.json")
	}
	return &Service{
		HTTPClient:      httpClient,
		APIURL:          defaultAPIURL,
		CachePath:       cachePath,
		Now:             time.Now,
		CacheTTL:        defaultCacheTTL,
		RetryTTL:        defaultRetryTTL,
		NotificationTTL: defaultNotificationTTL,
		Executable:      os.Executable,
		RunCommand:      runCommand,
	}
}

func (s *Service) Check(ctx context.Context, current string) (CheckResult, error) {
	result, _, _, err := s.check(ctx, current, true)
	return result, err
}

func (s *Service) Notification(ctx context.Context, current string) (CheckResult, bool, error) {
	currentVersion, ok := normalizeStableVersion(current)
	if !ok {
		return CheckResult{}, false, fmt.Errorf("%w (running version %q)", ErrDevelopmentBuild, current)
	}
	disabled := CheckResult{CurrentVersion: currentVersion}
	if s.CachePath == "" {
		return disabled, false, nil
	}

	releaseLock, acquired, err := acquireCacheLock(s.CachePath)
	if err != nil {
		return disabled, false, fmt.Errorf("lock update check cache: %w", err)
	}
	if !acquired {
		return disabled, false, nil
	}
	defer releaseLock()

	result, state, _, err := s.checkWithPersistence(ctx, currentVersion, false, true)
	if err != nil || !result.Known || !result.UpdateAvailable {
		return result, false, err
	}

	now := s.now()
	if state.NotifiedVersion == result.LatestVersion && recent(now, state.NotifiedAt, s.notificationTTL()) {
		return result, false, nil
	}
	state.NotifiedVersion = result.LatestVersion
	state.NotifiedAt = now
	if executable, executableErr := s.executablePath(); executableErr == nil {
		result.Channel, _ = installChannel(executable)
	}
	if err := s.writeCache(state); err != nil {
		return result, false, fmt.Errorf("persist update notification: %w", err)
	}
	return result, true, nil
}

func (s *Service) check(ctx context.Context, current string, force bool) (CheckResult, cacheState, release, error) {
	return s.checkWithPersistence(ctx, current, force, false)
}

func (s *Service) checkWithPersistence(ctx context.Context, current string, force, requirePersistence bool) (CheckResult, cacheState, release, error) {
	currentVersion, ok := normalizeStableVersion(current)
	if !ok {
		return CheckResult{}, cacheState{}, release{}, fmt.Errorf("%w (running version %q)", ErrDevelopmentBuild, current)
	}

	now := s.now()
	state := s.loadCache()
	cached := checkResult(currentVersion, state.LatestVersion)
	if !force {
		if cached.Known && recent(now, state.CheckedAt, s.cacheTTL()) {
			return cached, state, release{Version: cached.LatestVersion}, nil
		}
		if recent(now, state.AttemptedAt, s.retryTTL()) {
			return cached, state, release{Version: cached.LatestVersion}, nil
		}
	}

	etag := ""
	if !force {
		etag = state.ETag
	}
	latest, responseETag, notModified, err := s.fetchLatest(ctx, etag)
	state.AttemptedAt = now
	if err != nil {
		if persistErr := s.persistCache(state, requirePersistence); persistErr != nil {
			return CheckResult{CurrentVersion: currentVersion}, state, release{}, errors.Join(err, persistErr)
		}
		if !force && cached.Known {
			return cached, state, release{Version: cached.LatestVersion}, nil
		}
		return CheckResult{CurrentVersion: currentVersion}, state, release{}, err
	}
	if notModified {
		if !cached.Known {
			if persistErr := s.persistCache(state, requirePersistence); persistErr != nil {
				return CheckResult{CurrentVersion: currentVersion}, state, release{}, persistErr
			}
			return CheckResult{CurrentVersion: currentVersion}, state, release{}, errors.New("release server returned not modified without a cached release")
		}
		state.CheckedAt = now
		if err := s.persistCache(state, requirePersistence); err != nil {
			return cached, state, release{Version: cached.LatestVersion}, err
		}
		return cached, state, release{Version: cached.LatestVersion}, nil
	}

	state.LatestVersion = latest.Version
	state.ETag = responseETag
	state.CheckedAt = now
	result := checkResult(currentVersion, latest.Version)
	if err := s.persistCache(state, requirePersistence); err != nil {
		return result, state, latest, err
	}
	return result, state, latest, nil
}

func (s *Service) persistCache(state cacheState, required bool) error {
	err := s.writeCache(state)
	if err != nil && required {
		return fmt.Errorf("persist update check: %w", err)
	}
	return nil
}

func (s *Service) fetchLatest(ctx context.Context, etag string) (release, string, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.APIURL, nil)
	if err != nil {
		return release{}, "", false, fmt.Errorf("create release request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", updateCheckUserAgent)
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}

	response, err := s.httpClient().Do(request)
	if err != nil {
		return release{}, "", false, fmt.Errorf("check latest release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return release{}, response.Header.Get("ETag"), true, nil
	}
	if response.StatusCode != http.StatusOK {
		return release{}, "", false, fmt.Errorf("check latest release: HTTP %d", response.StatusCode)
	}

	body, err := readLimited(response.Body, response.ContentLength, maxReleaseMetadataBytes)
	if err != nil {
		return release{}, "", false, fmt.Errorf("read latest release: %w", err)
	}
	var payload releasePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return release{}, "", false, fmt.Errorf("decode latest release: %w", err)
	}
	version, ok := normalizeStableVersion(payload.TagName)
	if !ok || payload.Draft || payload.Prerelease {
		return release{}, "", false, fmt.Errorf("latest release has unsupported tag %q", payload.TagName)
	}
	return release{Version: version, Assets: payload.Assets}, response.Header.Get("ETag"), false, nil
}

func checkResult(current, latest string) CheckResult {
	normalizedLatest, ok := normalizeStableVersion(latest)
	if !ok {
		return CheckResult{CurrentVersion: current}
	}
	return CheckResult{
		CurrentVersion:  current,
		LatestVersion:   normalizedLatest,
		UpdateAvailable: newerVersion(normalizedLatest, current),
		Known:           true,
	}
}

func acquireCacheLock(cachePath string) (release func(), acquired bool, err error) {
	if info, statErr := os.Lstat(cachePath); statErr == nil {
		if !info.Mode().IsRegular() {
			return nil, false, errors.New("update check cache path is not a regular file")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, false, fmt.Errorf("inspect update check cache: %w", statErr)
	}

	directory := filepath.Dir(cachePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, false, fmt.Errorf("create cache directory: %w", err)
	}

	lockPath := cachePath + ".lock"
	return tryCacheLock(lockPath)
}

func (s *Service) loadCache() cacheState {
	if s.CachePath == "" {
		return cacheState{SchemaVersion: updateCacheSchemaVersion}
	}
	file, err := os.Open(s.CachePath)
	if err != nil {
		return cacheState{SchemaVersion: updateCacheSchemaVersion}
	}
	defer file.Close()
	data, err := readLimited(file, -1, 64<<10)
	if err != nil {
		return cacheState{SchemaVersion: updateCacheSchemaVersion}
	}
	var state cacheState
	if json.Unmarshal(data, &state) != nil || state.SchemaVersion != updateCacheSchemaVersion {
		return cacheState{SchemaVersion: updateCacheSchemaVersion}
	}
	return state
}

func (s *Service) writeCache(state cacheState) error {
	if s.CachePath == "" {
		return nil
	}
	state.SchemaVersion = updateCacheSchemaVersion
	directory := filepath.Dir(s.CachePath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".update-check-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(temporary).Encode(state); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.CachePath); err != nil {
		return err
	}
	keep = true
	return nil
}

func (s *Service) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return http.DefaultClient
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) cacheTTL() time.Duration {
	if s.CacheTTL > 0 {
		return s.CacheTTL
	}
	return defaultCacheTTL
}

func (s *Service) retryTTL() time.Duration {
	if s.RetryTTL > 0 {
		return s.RetryTTL
	}
	return defaultRetryTTL
}

func (s *Service) notificationTTL() time.Duration {
	if s.NotificationTTL > 0 {
		return s.NotificationTTL
	}
	return defaultNotificationTTL
}

func recent(now, then time.Time, interval time.Duration) bool {
	if then.IsZero() {
		return false
	}
	elapsed := now.Sub(then)
	return elapsed >= 0 && elapsed < interval
}

func readLimited(reader io.Reader, contentLength, maximum int64) ([]byte, error) {
	if contentLength > maximum {
		return nil, fmt.Errorf("response exceeds %d bytes", maximum)
	}
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("response exceeds %d bytes", maximum)
	}
	return data, nil
}

func cleanCommandOutput(output []byte) string {
	if len(output) > maxCommandOutput {
		output = output[len(output)-maxCommandOutput:]
	}
	return strings.TrimSpace(string(output))
}
