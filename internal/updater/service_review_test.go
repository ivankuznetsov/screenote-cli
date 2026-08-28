package updater

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewServicePinsReleaseAPIURL(t *testing.T) {
	t.Setenv("SCREENOTE_RELEASES_API_URL", "https://attacker.invalid/releases/latest")

	service := NewService(nil)
	if service.APIURL != defaultAPIURL {
		t.Fatalf("APIURL = %q, want %q", service.APIURL, defaultAPIURL)
	}
}

func TestNotificationSerializesSharedCacheWithoutWaiting(t *testing.T) {
	var requests atomic.Int32
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseResponse) }) }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		startOnce.Do(func() { close(requestStarted) })
		<-releaseResponse
		_, _ = io.WriteString(w, `{"tag_name":"v2.0.0","draft":false,"prerelease":false,"assets":[]}`)
	}))
	defer server.Close()
	defer release()

	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	newService := func() *Service {
		service := NewService(server.Client())
		service.APIURL = server.URL
		service.CachePath = cachePath
		service.Now = func() time.Time { return now }
		service.Executable = func() (string, error) {
			return filepath.Join(filepath.Dir(cachePath), "bin", "screenote"), nil
		}
		return service
	}

	type callResult struct {
		result CheckResult
		due    bool
		err    error
	}
	results := make(chan callResult, 2)
	call := func(service *Service) {
		result, due, err := service.Notification(context.Background(), "1.0.0")
		results <- callResult{result: result, due: due, err: err}
	}

	go call(newService())
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("first notification did not reach the release server")
	}

	go call(newService())
	var firstResult callResult
	select {
	case firstResult = <-results:
	case <-time.After(time.Second):
		t.Fatal("contending notification waited for the cache lock")
	}
	if firstResult.err != nil || firstResult.due {
		t.Fatalf("contending notification = (%#v, %v, %v)", firstResult.result, firstResult.due, firstResult.err)
	}

	release()
	var secondResult callResult
	select {
	case secondResult = <-results:
	case <-time.After(time.Second):
		t.Fatal("first notification did not finish")
	}
	if secondResult.err != nil || !secondResult.due {
		t.Fatalf("winning notification = (%#v, %v, %v)", secondResult.result, secondResult.due, secondResult.err)
	}
	if secondResult.result.Channel != "direct" {
		t.Fatalf("notification channel = %q, want direct", secondResult.result.Channel)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("release requests = %d, want 1", got)
	}
}

func TestNotificationWithEmptyCachePathSkipsNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"tag_name":"v2.0.0","assets":[]}`)
	}))
	defer server.Close()

	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = ""
	result, due, err := service.Notification(context.Background(), "1.0.0")
	if err != nil || due {
		t.Fatalf("notification = (%#v, %v, %v)", result, due, err)
	}
	if result.CurrentVersion != "1.0.0" {
		t.Fatalf("current version = %q", result.CurrentVersion)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("release requests = %d, want 0", got)
	}
}

func TestNotificationCacheDirectoryFailurePreventsNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"tag_name":"v2.0.0","assets":[]}`)
	}))
	defer server.Close()

	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block cache directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = filepath.Join(blocker, "update-check.json")
	_, due, err := service.Notification(context.Background(), "1.0.0")
	if err == nil || due {
		t.Fatalf("notification = (due %v, error %v), want a cache error", due, err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("release requests = %d, want 0", got)
	}
}

func TestNotificationRequiresPersistenceButExplicitOperationsDoNot(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"tag_name":"v2.0.0","draft":false,"prerelease":false,"assets":[]}`)
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	if err := os.Mkdir(cachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = cachePath

	result, due, err := service.Notification(context.Background(), "1.0.0")
	if err == nil || due {
		t.Fatalf("notification = (%#v, %v, %v), want persistence failure", result, due, err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("automatic notification made %d requests before validating cache persistence, want 0", got)
	}

	result, err = service.Check(context.Background(), "1.0.0")
	if err != nil {
		t.Fatalf("explicit check failed because cache is unavailable: %v", err)
	}
	if !result.UpdateAvailable || result.LatestVersion != "2.0.0" {
		t.Fatalf("explicit check result = %#v", result)
	}

	updateResult, err := service.Update(context.Background(), "2.0.0", nil)
	if err != nil {
		t.Fatalf("explicit update check failed because cache is unavailable: %v", err)
	}
	if updateResult.Updated || updateResult.LatestVersion != "2.0.0" {
		t.Fatalf("explicit update result = %#v", updateResult)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("release requests = %d, want 2", got)
	}
}

func TestNotificationRetriesFailedCheckAfterTwentyFourHours(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = filepath.Join(t.TempDir(), "update-check.json")
	service.Now = func() time.Time { return now }

	if _, _, err := service.Notification(context.Background(), "1.0.0"); err == nil {
		t.Fatal("first failed check returned nil error")
	}
	now = now.Add(23 * time.Hour)
	if result, due, err := service.Notification(context.Background(), "1.0.0"); err != nil || result.Known || due {
		t.Fatalf("23-hour backoff result = (%#v, %v, %v)", result, due, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("23-hour backoff made %d requests, want 1", got)
	}

	now = now.Add(time.Hour)
	if _, _, err := service.Notification(context.Background(), "1.0.0"); err == nil {
		t.Fatal("24-hour retry returned nil error")
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("24-hour retry made %d requests, want 2", got)
	}
}

func TestNotificationIgnoresUnlockedCacheLockFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v2.0.0","draft":false,"prerelease":false,"assets":[]}`)
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	lockPath := cachePath + ".lock"
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = cachePath
	result, due, err := service.Notification(context.Background(), "1.0.0")
	if err != nil || !due || !result.UpdateAvailable {
		t.Fatalf("notification = (%#v, %v, %v)", result, due, err)
	}
	if info, err := os.Stat(lockPath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("cache lock file = (%v, %v), want a reusable regular file", info, err)
	}
}
