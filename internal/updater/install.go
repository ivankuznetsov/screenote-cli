package updater

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const (
	brewFormula         = "ivankuznetsov/tap/screenote"
	directDownloadBase  = "https://github.com/ivankuznetsov/screenote-cli/releases/download"
	directInstallerPath = "/usr/bin:/bin:/usr/sbin:/sbin"
	maxInstallerBytes   = 1 << 20
	maxCommandOutput    = 2 << 10
)

type CommandRunner func(ctx context.Context, name string, args, env []string, stdin io.Reader) ([]byte, error)

type UpdateResult struct {
	CheckResult
	Updated bool
}

func (s *Service) Update(ctx context.Context, current string, stdin io.Reader) (UpdateResult, error) {
	checked, _, latest, err := s.check(ctx, current, true)
	result := UpdateResult{CheckResult: checked}
	if err != nil {
		return result, err
	}
	if !checked.UpdateAvailable {
		return result, nil
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return result, fmt.Errorf("automatic update is unsupported on %s", runtime.GOOS)
	}

	executable, err := s.executablePath()
	if err != nil {
		return result, fmt.Errorf("locate running executable: %w", err)
	}
	channel, brew := installChannel(executable)
	result.Channel = channel
	if channel == "package" {
		return result, fmt.Errorf("screenote at %s appears to be package-managed; update it with its package manager", executable)
	}
	if channel == "brew" {
		runner := s.commandRunner()
		output, runErr := runner(ctx, brew, []string{"upgrade", brewFormula}, os.Environ(), stdin)
		if runErr != nil {
			return result, commandError("Homebrew updater", output, runErr)
		}
		output, runErr = runner(ctx, brew, []string{"list", "--versions", brewFormula}, os.Environ(), nil)
		if runErr != nil {
			return result, commandError("verify Homebrew update", output, runErr)
		}
		if !brewOutputHasVersion(output, latest.Version) {
			installed := cleanCommandOutput(output)
			if installed == "" {
				installed = "no installed version reported"
			}
			return result, fmt.Errorf("Homebrew did not install Screenote %s (%s); run `brew update` and retry", latest.Version, installed)
		}
		result.Updated = true
		return result, nil
	}

	installer, err := installerAsset(latest)
	if err != nil {
		return result, err
	}
	contents, err := s.downloadInstaller(ctx, installer)
	if err != nil {
		return result, err
	}
	temporaryDirectory, err := os.MkdirTemp("", "screenote-update-*")
	if err != nil {
		return result, fmt.Errorf("create updater temporary directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	installerPath := filepath.Join(temporaryDirectory, "install.sh")
	if err := writeInstaller(installerPath, contents); err != nil {
		return result, err
	}

	environment := withEnvironment(os.Environ(), map[string]string{
		"PATH":                    directInstallerPath,
		"SCREENOTE_DOWNLOAD_BASE": directDownloadBase,
		"SCREENOTE_VERSION":       latest.Version,
		"SCREENOTE_INSTALL_DIR":   filepath.Dir(executable),
	})
	output, runErr := s.commandRunner()(ctx, "/bin/sh", []string{installerPath}, environment, stdin)
	if runErr != nil {
		return result, commandError("direct updater", output, runErr)
	}
	result.Updated = true
	return result, nil
}

func (s *Service) downloadInstaller(ctx context.Context, asset releaseAsset) ([]byte, error) {
	expected, err := installerDigest(asset.Digest)
	if err != nil {
		return nil, err
	}
	if asset.Size > maxInstallerBytes {
		return nil, fmt.Errorf("release installer exceeds %d bytes", maxInstallerBytes)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("create installer request: %w", err)
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", updateCheckUserAgent)
	response, err := s.httpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("download release installer: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download release installer: HTTP %d", response.StatusCode)
	}
	contents, err := readLimited(response.Body, response.ContentLength, maxInstallerBytes)
	if err != nil {
		return nil, fmt.Errorf("read release installer: %w", err)
	}
	if asset.Size > 0 && int64(len(contents)) != asset.Size {
		return nil, fmt.Errorf("release installer size mismatch: got %d, want %d", len(contents), asset.Size)
	}
	actual := sha256.Sum256(contents)
	if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
		return nil, errors.New("release installer digest verification failed")
	}
	return contents, nil
}

func installerAsset(latest release) (releaseAsset, error) {
	for _, asset := range latest.Assets {
		if asset.Name == "install.sh" && asset.URL != "" {
			return asset, nil
		}
	}
	return releaseAsset{}, errors.New("latest release does not contain install.sh")
}

func installerDigest(value string) ([]byte, error) {
	algorithm, encoded, ok := strings.Cut(value, ":")
	if !ok || algorithm != "sha256" || len(encoded) != sha256.Size*2 {
		return nil, errors.New("release installer is missing a valid SHA-256 digest")
	}
	digest, err := hex.DecodeString(encoded)
	if err != nil || len(digest) != sha256.Size {
		return nil, errors.New("release installer is missing a valid SHA-256 digest")
	}
	return digest, nil
}

func writeInstaller(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return fmt.Errorf("create release installer: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write release installer: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync release installer: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close release installer: %w", err)
	}
	return nil
}

func (s *Service) executablePath() (string, error) {
	resolver := s.Executable
	if resolver == nil {
		resolver = os.Executable
	}
	path, err := resolver()
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		return resolved, nil
	}
	return absolute, nil
}

func installChannel(executable string) (string, string) {
	normalized := filepath.ToSlash(executable)
	cellar := strings.Index(normalized, "/Cellar/screenote/")
	if cellar >= 0 {
		brew := filepath.FromSlash(normalized[:cellar] + "/bin/brew")
		if info, err := os.Stat(brew); err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return "package", ""
		}
		return "brew", brew
	}
	if normalized == "/usr/bin/screenote" || normalized == "/opt/local/bin/screenote" || strings.HasPrefix(normalized, "/nix/store/") || strings.HasPrefix(normalized, "/snap/") {
		return "package", ""
	}
	return "direct", ""
}

func brewOutputHasVersion(output []byte, wanted string) bool {
	for _, field := range strings.Fields(string(output)) {
		version, ok := normalizeStableVersion(field)
		if ok && version == wanted {
			return true
		}
	}
	return false
}

func (s *Service) commandRunner() CommandRunner {
	if s.RunCommand != nil {
		return s.RunCommand
	}
	return runCommand
}

func runCommand(ctx context.Context, name string, args, environment []string, stdin io.Reader) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = environment
	command.Stdin = stdin
	output := &tailWriter{limit: maxCommandOutput}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	return output.Bytes(), err
}

type tailWriter struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (w *tailWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := len(data)
	if len(data) >= w.limit {
		w.data = append(w.data[:0], data[len(data)-w.limit:]...)
		return written, nil
	}
	w.data = append(w.data, data...)
	if excess := len(w.data) - w.limit; excess > 0 {
		copy(w.data, w.data[excess:])
		w.data = w.data[:w.limit]
	}
	return written, nil
}

func (w *tailWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.data...)
}

func withEnvironment(environment []string, overrides map[string]string) []string {
	result := make([]string, 0, len(environment)+len(overrides))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[key]; !overridden {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func commandError(label string, output []byte, err error) error {
	detail := cleanCommandOutput(output)
	if detail == "" {
		return fmt.Errorf("%s failed: %w", label, err)
	}
	return fmt.Errorf("%s failed: %w: %s", label, err, detail)
}
