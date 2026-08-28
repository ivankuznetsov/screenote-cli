package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewerVersionUsesNumericStableVersionPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		latest  string
		current string
		want    bool
	}{
		{name: "minor", latest: "1.10.0", current: "1.9.9", want: true},
		{name: "equal", latest: "v1.2.3", current: "1.2.3", want: false},
		{name: "local ahead", latest: "1.2.3", current: "2.0.0", want: false},
		{name: "leading zero invalid", latest: "1.02.0", current: "1.1.0", want: false},
		{name: "prerelease outside release contract", latest: "1.2.0-rc.1", current: "1.1.0", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := newerVersion(test.latest, test.current); got != test.want {
				t.Fatalf("newerVersion(%q, %q) = %v, want %v", test.latest, test.current, got, test.want)
			}
		})
	}
}

func TestCheckCachesLatestReleaseAndUsesETagAfterTTL(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := requests.Add(1)
		if request == 2 {
			if got := r.Header.Get("If-None-Match"); got != `"release-1"` {
				t.Fatalf("If-None-Match = %q", got)
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"release-1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tag_name":"v1.10.0","draft":false,"prerelease":false,"assets":[]}`)
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "screenote", "update-check.json")
	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = cachePath
	service.Now = func() time.Time { return now }

	result, _, _, err := service.check(context.Background(), "1.9.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Known || !result.UpdateAvailable || result.LatestVersion != "1.10.0" {
		t.Fatalf("result = %#v", result)
	}

	now = now.Add(time.Hour)
	if _, _, _, err := service.check(context.Background(), "1.9.0", false); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("fresh cache made %d requests, want 1", got)
	}

	now = now.Add(24 * time.Hour)
	result, _, _, err = service.check(context.Background(), "1.9.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.LatestVersion != "1.10.0" || !result.UpdateAvailable {
		t.Fatalf("304 result = %#v", result)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("stale cache made %d requests, want 2", got)
	}

	info, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cache mode = %#o, want 0600", got)
	}
}

func TestNotificationIsShownAtMostOncePerInterval(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tag_name":"v2.0.0","draft":false,"prerelease":false,"assets":[]}`)
	}))
	defer server.Close()

	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = filepath.Join(t.TempDir(), "update-check.json")
	service.Now = func() time.Time { return now }

	result, due, err := service.Notification(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !due || !result.UpdateAvailable {
		t.Fatalf("first notification = (%#v, %v)", result, due)
	}

	now = now.Add(time.Hour)
	_, due, err = service.Notification(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Fatal("notification repeated inside interval")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("fresh cache made %d requests, want 1", got)
	}

	now = now.Add(24 * time.Hour)
	_, due, err = service.Notification(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("notification was not repeated after interval")
	}
}

func TestNotificationBacksOffAfterFailedCheck(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	now = now.Add(time.Hour)
	result, due, err := service.Notification(context.Background(), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if result.Known || due {
		t.Fatalf("backoff result = (%#v, %v)", result, due)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("failure backoff made %d requests, want 1", got)
	}
}

func TestForcedCheckDoesNotHideNetworkFailureBehindCache(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	failing := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"tag_name":"v1.2.0","draft":false,"prerelease":false,"assets":[]}`)
	}))
	defer server.Close()

	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = filepath.Join(t.TempDir(), "update-check.json")
	service.Now = func() time.Time { return now }
	if _, _, _, err := service.check(context.Background(), "1.0.0", false); err != nil {
		t.Fatal(err)
	}

	failing = true
	if _, err := service.Check(context.Background(), "1.0.0"); err == nil {
		t.Fatal("forced check returned stale cache instead of the network error")
	}
}

func TestOversizedCacheIsRejectedWithoutPreventingRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v1.2.0","draft":false,"prerelease":false,"assets":[]}`)
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	if err := os.WriteFile(cachePath, []byte(strings.Repeat("x", 65<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = cachePath

	result, _, _, err := service.check(context.Background(), "1.0.0", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Known || result.LatestVersion != "1.2.0" {
		t.Fatalf("result = %#v", result)
	}
}

func TestUpdateVerifiesInstallerDigestAndPinsTrustedEnvironmentAndResolvedDirectory(t *testing.T) {
	installer := []byte("#!/bin/sh\nexit 0\n")
	digest := sha256.Sum256(installer)
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"tag_name":"v1.2.0","draft":false,"prerelease":false,"assets":[{"name":"install.sh","browser_download_url":%q,"size":%d,"digest":"sha256:%s"}]}`,
				serverURL+"/install.sh", len(installer), hex.EncodeToString(digest[:]))
		case "/install.sh":
			_, _ = w.Write(installer)
		default:
			http.NotFound(w, r)
		}
	}))
	serverURL = server.URL
	defer server.Close()

	root := t.TempDir()
	resolvedExecutable := filepath.Join(root, "resolved", "bin", "screenote")
	if err := os.MkdirAll(filepath.Dir(resolvedExecutable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolvedExecutable, []byte("existing binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "linked", "screenote")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(resolvedExecutable, executable); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/tmp/hostile-bin")
	t.Setenv("SCREENOTE_DOWNLOAD_BASE", "https://attacker.invalid/releases")
	service := NewService(server.Client())
	service.APIURL = server.URL + "/latest"
	service.CachePath = filepath.Join(t.TempDir(), "update-check.json")
	service.Executable = func() (string, error) { return executable, nil }
	var ran bool
	service.RunCommand = func(_ context.Context, name string, args, env []string, _ io.Reader) ([]byte, error) {
		ran = true
		if name != "/bin/sh" || len(args) != 1 {
			t.Fatalf("command = %q %#v", name, args)
		}
		got, err := os.ReadFile(args[0])
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(installer) {
			t.Fatalf("installer = %q", got)
		}
		if got := envValue(env, "SCREENOTE_VERSION"); got != "1.2.0" {
			t.Fatalf("SCREENOTE_VERSION = %q", got)
		}
		if got := envValue(env, "SCREENOTE_INSTALL_DIR"); got != filepath.Dir(resolvedExecutable) {
			t.Fatalf("SCREENOTE_INSTALL_DIR = %q", got)
		}
		if got := envValue(env, "SCREENOTE_DOWNLOAD_BASE"); got != directDownloadBase {
			t.Fatalf("SCREENOTE_DOWNLOAD_BASE = %q", got)
		}
		if got := envValue(env, "PATH"); got != directInstallerPath {
			t.Fatalf("PATH = %q", got)
		}
		return []byte("installed"), nil
	}

	result, err := service.Update(context.Background(), "1.1.0", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if !ran || !result.Updated || result.Channel != "direct" || result.LatestVersion != "1.2.0" {
		t.Fatalf("result = %#v, ran = %v", result, ran)
	}
}

func TestUpdateRejectsInstallerWithWrongDigest(t *testing.T) {
	installer := []byte("#!/bin/sh\nexit 0\n")
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_, _ = fmt.Fprintf(w, `{"tag_name":"v1.2.0","assets":[{"name":"install.sh","browser_download_url":%q,"size":%d,"digest":"sha256:%s"}]}`,
				serverURL+"/install.sh", len(installer), strings.Repeat("0", 64))
		case "/install.sh":
			_, _ = w.Write(installer)
		}
	}))
	serverURL = server.URL
	defer server.Close()

	service := NewService(server.Client())
	service.APIURL = server.URL + "/latest"
	service.CachePath = filepath.Join(t.TempDir(), "update-check.json")
	service.Executable = func() (string, error) { return "/tmp/bin/screenote", nil }
	service.RunCommand = func(context.Context, string, []string, []string, io.Reader) ([]byte, error) {
		t.Fatal("installer ran before its digest was verified")
		return nil, nil
	}

	if _, err := service.Update(context.Background(), "1.1.0", strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpdatePreservesHomebrewInstallChannel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v1.2.0","draft":false,"prerelease":false,"assets":[]}`)
	}))
	defer server.Close()

	root := t.TempDir()
	prefix := filepath.Join(root, "prefix")
	resolvedExecutable := filepath.Join(prefix, "Cellar", "screenote", "1.1.0", "bin", "screenote")
	if err := os.MkdirAll(filepath.Dir(resolvedExecutable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolvedExecutable, []byte("existing binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	brew := filepath.Join(prefix, "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "linked", "screenote")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(resolvedExecutable, executable); err != nil {
		t.Fatal(err)
	}
	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = filepath.Join(t.TempDir(), "update-check.json")
	service.Executable = func() (string, error) { return executable, nil }
	var commands []string
	service.RunCommand = func(_ context.Context, name string, args, _ []string, _ io.Reader) ([]byte, error) {
		commands = append(commands, strings.Join(args, " "))
		if name != brew {
			t.Fatalf("command name = %q, want %q", name, brew)
		}
		switch strings.Join(args, " ") {
		case "upgrade ivankuznetsov/tap/screenote":
			return []byte("upgraded"), nil
		case "list --versions ivankuznetsov/tap/screenote":
			return []byte("screenote 1.2.0\n"), nil
		default:
			t.Fatalf("command args = %#v", args)
			return nil, nil
		}
	}

	result, err := service.Update(context.Background(), "1.1.0", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.Channel != "brew" {
		t.Fatalf("result = %#v", result)
	}
	if got := strings.Join(commands, "; "); got != "upgrade ivankuznetsov/tap/screenote; list --versions ivankuznetsov/tap/screenote" {
		t.Fatalf("commands = %q", got)
	}
}

func TestUpdateRejectsStaleHomebrewTapAfterUpgrade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v1.2.0","draft":false,"prerelease":false,"assets":[]}`)
	}))
	defer server.Close()

	root := t.TempDir()
	executable := filepath.Join(root, "Cellar", "screenote", "1.1.0", "bin", "screenote")
	brew := filepath.Join(root, "bin", "brew")
	if err := os.MkdirAll(filepath.Dir(brew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brew, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := NewService(server.Client())
	service.APIURL = server.URL
	service.CachePath = filepath.Join(t.TempDir(), "update-check.json")
	service.Executable = func() (string, error) { return executable, nil }
	service.RunCommand = func(_ context.Context, _ string, args, _ []string, _ io.Reader) ([]byte, error) {
		if strings.Join(args, " ") == "list --versions ivankuznetsov/tap/screenote" {
			return []byte("screenote 1.1.0\n"), nil
		}
		return []byte("already upgraded"), nil
	}

	result, err := service.Update(context.Background(), "1.1.0", strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "did not install Screenote 1.2.0") || !strings.Contains(err.Error(), "brew update") {
		t.Fatalf("error = %v", err)
	}
	if result.Updated {
		t.Fatalf("result = %#v", result)
	}
}

func TestUpdateRefusesToOverwriteSystemPackageInstall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v1.2.0","draft":false,"prerelease":false,"assets":[]}`)
	}))
	defer server.Close()

	missingBrew := filepath.Join(t.TempDir(), "Cellar", "screenote", "1.1.0", "bin", "screenote")
	t.Setenv("PATH", t.TempDir())
	for _, executable := range []string{"/usr/bin/screenote", "/opt/local/bin/screenote", missingBrew} {
		t.Run(executable, func(t *testing.T) {
			service := NewService(server.Client())
			service.APIURL = server.URL
			service.CachePath = filepath.Join(t.TempDir(), "update-check.json")
			service.Executable = func() (string, error) { return executable, nil }
			service.RunCommand = func(context.Context, string, []string, []string, io.Reader) ([]byte, error) {
				t.Fatal("package-managed executable was overwritten")
				return nil, nil
			}

			_, err := service.Update(context.Background(), "1.1.0", strings.NewReader(""))
			if err == nil || !strings.Contains(err.Error(), "package manager") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestTailWriterKeepsOnlyBoundedTrailingOutput(t *testing.T) {
	writer := &tailWriter{limit: 5}
	if written, err := writer.Write([]byte("1234")); err != nil || written != 4 {
		t.Fatalf("first write = (%d, %v)", written, err)
	}
	if written, err := writer.Write([]byte("567890")); err != nil || written != 6 {
		t.Fatalf("second write = (%d, %v)", written, err)
	}
	if got := string(writer.Bytes()); got != "67890" {
		t.Fatalf("tail = %q", got)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}
