package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolvePrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(path, Values{Token: "file-token", BaseURL: "http://file", Project: "file-project"}); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(Options{
		ConfigPath: path,
		Flags:      Values{Token: "flag-token"},
		Env: map[string]string{
			"SCREENOTE_TOKEN":    "env-token",
			"SCREENOTE_BASE_URL": "http://env",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if resolved.Token != "flag-token" || resolved.Sources.Token != "flag" {
		t.Fatalf("token = %q from %q", resolved.Token, resolved.Sources.Token)
	}
	if resolved.BaseURL != "http://env" || resolved.Sources.BaseURL != "env" {
		t.Fatalf("base url = %q from %q", resolved.BaseURL, resolved.Sources.BaseURL)
	}
	if resolved.Project != "file-project" || resolved.Sources.Project != "config" {
		t.Fatalf("project = %q from %q", resolved.Project, resolved.Sources.Project)
	}
}

func TestResolveTokenSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(path, Values{Token: "file-token"}); err != nil {
		t.Fatal(err)
	}

	envResolved, err := Resolve(Options{
		ConfigPath: path,
		Env:        map[string]string{"SCREENOTE_TOKEN": "env-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if envResolved.Token != "env-token" || envResolved.Sources.Token != "env" {
		t.Fatalf("token = %q from %q", envResolved.Token, envResolved.Sources.Token)
	}

	fileResolved, err := Resolve(Options{ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if fileResolved.Token != "file-token" || fileResolved.Sources.Token != "config" {
		t.Fatalf("token = %q from %q", fileResolved.Token, fileResolved.Sources.Token)
	}
}

func TestResolveDefaultsToHostedScreenote(t *testing.T) {
	resolved, err := Resolve(Options{
		ConfigPath: filepath.Join(t.TempDir(), "missing.toml"),
		Env:        map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.BaseURL != DefaultBaseURL || resolved.Sources.BaseURL != "default" {
		t.Fatalf("base url = %q from %q", resolved.BaseURL, resolved.Sources.BaseURL)
	}
}

func TestResolveBaseURLOverridesHostedDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := Save(path, Values{BaseURL: "https://self-hosted.example"}); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(Options{ConfigPath: path, Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.BaseURL != "https://self-hosted.example" || resolved.Sources.BaseURL != "config" {
		t.Fatalf("base url = %q from %q", resolved.BaseURL, resolved.Sources.BaseURL)
	}
}

func TestResolveIgnoresLegacyAPIKeySources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("api_key = \"legacy\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(Options{
		ConfigPath: path,
		Env:        map[string]string{"SCREENOTE_API_KEY": "legacy-env"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Token != "" || resolved.Sources.Token != "" {
		t.Fatalf("legacy API key source resolved token: %#v", resolved)
	}
}

func TestLoadMissingConfig(t *testing.T) {
	values, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if values != (Values{}) {
		t.Fatalf("values = %#v", values)
	}
}

func TestSaveTightensExistingConfigPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not available on Windows")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("base_url = \"https://old.example\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, Values{BaseURL: "https://screenote.test", Login: &LoginCredentials{AccessToken: "access-1"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("permissions=%#o want 0600", got)
	}
}
