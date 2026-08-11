package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	appconfig "github.com/ivankuznetsov/screenote-cli/internal/config"
)

func TestScreenshotListSendsFilters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/7/screenshots" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.URL.Query().Get("page_id") != "9" || r.URL.Query().Get("status") != "ready" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"screenshots":[],"pagination":{"total":0,"limit":10,"offset":2}}`))
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "screenshot", "list", "--page", "9", "--status", "ready", "--limit", "10", "--offset", "2"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `"screenshots"`) {
		t.Fatalf("stdout=%s", stdout)
	}
}

func TestAnnotationGetPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/annotations/5" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("project_id") != "7" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"id":5}`))
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `"id":5`) {
		t.Fatalf("stdout=%s", stdout)
	}
}

func TestAnnotationGetPreservesRawCropByDefault(t *testing.T) {
	encoded, _ := cropPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":5,"cropped_image_base64":%q,"mime_type":"image/png"}`, encoded)
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `"cropped_image_base64":"`+encoded+`"`) {
		t.Fatalf("stdout=%s", stdout)
	}
}

func TestAnnotationGetExportsCropPrivatelyAndRedactsBase64(t *testing.T) {
	encoded, pngBytes := cropPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":5,"status":"open","cropped_image_base64":%q,"mime_type":"image/png"}`, encoded)
	}))
	defer server.Close()

	cropPath := filepath.Join(t.TempDir(), "annotation.png")
	if err := os.WriteFile(cropPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--crop-file", cropPath}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	written, err := os.ReadFile(cropPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, pngBytes) {
		t.Fatalf("crop bytes differ: got %d want %d", len(written), len(pngBytes))
	}
	info, err := os.Stat(cropPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("crop mode=%#o want 0600", got)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout=%s err=%v", stdout, err)
	}
	if _, exists := payload["cropped_image_base64"]; exists {
		t.Fatalf("base64 was not redacted: %s", stdout)
	}
	if payload["crop_file"] != cropPath || payload["id"] != float64(5) || payload["status"] != "open" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestAnnotationGetCropRejectsTraversalBeforeRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		t.Fatal("unexpected request")
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--crop-file", "../outside.png"}, "")
	if code != ExitUsage || !strings.Contains(stderr, `"code":"invalid_crop_file"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if called {
		t.Fatal("request was made for an unsafe path")
	}
}

func TestAnnotationGetCropRejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.png")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "crop.png")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		t.Fatal("unexpected request")
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--crop-file", link}, "")
	if code != ExitUsage || !strings.Contains(stderr, `"code":"invalid_crop_file"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if called {
		t.Fatal("request was made for a symlink target")
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != "keep" {
		t.Fatalf("target changed: contents=%q err=%v", contents, err)
	}
}

func TestAnnotationGetCropRejectsSymlinkTargetCreatedDuringRequest(t *testing.T) {
	encoded, _ := cropPNG(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.png")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	cropPath := filepath.Join(dir, "crop.png")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.Symlink(target, cropPath); err != nil {
			t.Errorf("create late symlink: %v", err)
			http.Error(w, "setup failed", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"id":5,"cropped_image_base64":%q,"mime_type":"image/png"}`, encoded)
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--crop-file", cropPath}, "")
	if code != ExitUsage || !strings.Contains(stderr, `"code":"invalid_crop_file"`) || stdout != "" {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != "keep" {
		t.Fatalf("target changed: contents=%q err=%v", contents, err)
	}
}

func TestAnnotationGetCropAllowsSymlinkedParentDirectory(t *testing.T) {
	encoded, pngBytes := cropPNG(t)
	realDir := t.TempDir()
	linkRoot := t.TempDir()
	linkedDir := filepath.Join(linkRoot, "linked")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":5,"cropped_image_base64":%q,"mime_type":"image/png"}`, encoded)
	}))
	defer server.Close()

	cropPath := filepath.Join(linkedDir, "crop.png")
	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--crop-file", cropPath}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	written, err := os.ReadFile(filepath.Join(realDir, "crop.png"))
	if err != nil || !bytes.Equal(written, pngBytes) {
		t.Fatalf("canonical crop differs: bytes=%d err=%v", len(written), err)
	}
	if !strings.Contains(stdout, `"crop_file":"`+cropPath+`"`) {
		t.Fatalf("stdout should retain requested path: %s", stdout)
	}
}

func TestAnnotationGetCropRejectsInvalidDataWithoutWriting(t *testing.T) {
	_, pngBytes := cropPNG(t)
	cases := map[string]string{
		"invalid base64":     "%%%",
		"not png":            base64.StdEncoding.EncodeToString([]byte("not a png")),
		"truncated png body": base64.StdEncoding.EncodeToString(pngBytes[:33]),
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"id":5,"cropped_image_base64":%q,"mime_type":"image/png"}`, encoded)
			}))
			defer server.Close()
			cropPath := filepath.Join(t.TempDir(), "crop.png")

			_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--crop-file", cropPath}, "")
			if code != ExitGeneric || !strings.Contains(stderr, `"code":"invalid_crop_data"`) {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			if _, err := os.Stat(cropPath); !os.IsNotExist(err) {
				t.Fatalf("crop file exists after invalid data: %v", err)
			}
		})
	}
}

func TestAnnotationGetCropRejectsOversizedDimensionsWithoutWriting(t *testing.T) {
	encoded, _ := cropPNGSize(t, 1073, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":5,"cropped_image_base64":%q,"mime_type":"image/png"}`, encoded)
	}))
	defer server.Close()
	cropPath := filepath.Join(t.TempDir(), "crop.png")

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--crop-file", cropPath}, "")
	if code != ExitGeneric || !strings.Contains(stderr, `"code":"crop_too_large"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if _, err := os.Stat(cropPath); !os.IsNotExist(err) {
		t.Fatalf("crop file exists after oversized data: %v", err)
	}
}

func TestAnnotationGetCropReportsUnavailableCrop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":5,"cropped_image_base64":null,"mime_type":"image/png"}`))
	}))
	defer server.Close()
	cropPath := filepath.Join(t.TempDir(), "crop.png")

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--crop-file", cropPath}, "")
	if code != ExitGeneric || !strings.Contains(stderr, `"code":"crop_unavailable"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestAnnotationGetCropReportsWriteFailureWithoutReplacingDestination(t *testing.T) {
	encoded, _ := cropPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":5,"cropped_image_base64":%q,"mime_type":"image/png"}`, encoded)
	}))
	defer server.Close()
	cropPath := filepath.Join(t.TempDir(), "destination")
	if err := os.Mkdir(cropPath, 0o700); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--crop-file", cropPath}, "")
	if code != ExitGeneric || !strings.Contains(stderr, `"code":"crop_write_failed"`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout should stay empty on write failure: %s", stdout)
	}
	info, err := os.Stat(cropPath)
	if err != nil || !info.IsDir() {
		t.Fatalf("destination was replaced: info=%v err=%v", info, err)
	}
}

func TestProjectCreatePathAndName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("name"); got != "Plugin review" {
			t.Fatalf("name=%q", got)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"project":{"id":8,"name":"Plugin review","role":"owner"}}`))
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "project", "create", "--name", "Plugin review"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `"id":8`) || !strings.Contains(stdout, `"role":"owner"`) {
		t.Fatalf("stdout=%s", stdout)
	}
}

func TestProjectCreateRequiresNameWithoutRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		t.Fatalf("unexpected request %s", r.URL.Path)
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "project", "create"}, "")
	if code != ExitUsage || !strings.Contains(stderr, `"code":"missing_name"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if called {
		t.Fatal("request was made before local validation")
	}
}

func TestProjectCreatePropagatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"Project limit reached","code":"project_limit"}`))
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "project", "create", "--name", "Another"}, "")
	if code != ExitGeneric || !strings.Contains(stderr, `"code":"project_limit"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestAnnotationResolvePathAndParams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/annotations/5/resolve" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("project_id"); got != "7" {
			t.Fatalf("project_id=%q", got)
		}
		if got := r.Form.Get("comment"); got != "Fixed in abc1234" {
			t.Fatalf("comment=%q", got)
		}
		_, _ = w.Write([]byte(`{"success":true,"annotation":{"id":5,"status":"resolved"},"comment":{"body":"Fixed in abc1234"},"operation":"resolved"}`))
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "resolve", "--annotation", "5", "--comment", "Fixed in abc1234"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, `"operation":"resolved"`) {
		t.Fatalf("stdout=%s", stdout)
	}
}

func TestAnnotationResolveAllowsOmittedComment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if _, present := r.Form["comment"]; present {
			t.Fatalf("comment should be omitted: %s", r.Form.Encode())
		}
		_, _ = w.Write([]byte(`{"success":true,"comment":null,"operation":"already_resolved"}`))
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "resolve", "--annotation", "5"}, "")
	if code != ExitOK || !strings.Contains(stdout, `"operation":"already_resolved"`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

func TestAnnotationResolveRequiresAnnotationAndProjectWithoutRequest(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code string
	}{
		{name: "annotation", args: []string{"--project", "7", "annotation", "resolve"}, code: "missing_annotation"},
		{name: "project", args: []string{"annotation", "resolve", "--annotation", "5"}, code: "missing_project"},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				t.Fatalf("unexpected request %s", r.URL.Path)
			}))
			defer server.Close()
			args := append([]string{"--base-url", server.URL, "--token", "key"}, test.args...)
			_, stderr, code := runCLI(t, args, "")
			if code != ExitUsage || !strings.Contains(stderr, `"code":"`+test.code+`"`) {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			if called {
				t.Fatal("request was made before local validation")
			}
		})
	}
}

func TestAnnotationResolvePropagatesNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Annotation not found","code":"not_found"}`))
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "resolve", "--annotation", "999"}, "")
	if code != ExitNotFound || !strings.Contains(stderr, `"code":"not_found"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestAnnotationListPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/screenshots/4/annotations" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("status") != "open" || r.URL.Query().Get("viewport") != "mobile" || r.URL.Query().Get("project_id") != "7" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"annotations":[]}`))
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "list", "--screenshot", "4", "--status", "open", "--viewport", "mobile"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestUnknownFlagExitsUsage(t *testing.T) {
	stdout, stderr, code := runCLI(t, []string{"--base-url", "http://example.test", "--token", "key", "project", "list", "--nope"}, "")
	if code != ExitUsage {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "invalid_flag") {
		t.Fatalf("stderr=%s", stderr)
	}
}

func TestLegacyAPIKeyFlagIsRejected(t *testing.T) {
	stdout, stderr, code := runCLI(t, []string{"--base-url", "http://example.test", "--api-key", "key", "project", "list"}, "")
	if code != ExitUsage {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "unknown flag: --api-key") {
		t.Fatalf("stderr=%s", stderr)
	}
}

func TestMissingTokenUsesStableUsageError(t *testing.T) {
	stdout, stderr, code := runCLI(t, []string{"--base-url", "http://example.test", "project", "list"}, "")
	if code != ExitUsage {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "missing_token") {
		t.Fatalf("stderr=%s", stderr)
	}
}

func TestConfigMasksBearerToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	stdout, stderr, code := runCLI(t, []string{
		"--config", configPath,
		"--base-url", "https://screenote.test",
		"--token", "super-secret-token",
		"config",
	}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stdout, "super-secret-token") {
		t.Fatalf("stdout leaked token: %s", stdout)
	}
	var payload struct {
		TokenSet bool `json:"token_set"`
		Sources  struct {
			Token string `json:"token"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout=%s err=%v", stdout, err)
	}
	if !payload.TokenSet || payload.Sources.Token != "flag" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestConfigReportsStoredOAuthLogin(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := appconfig.Save(configPath, appconfig.Values{
		BaseURL: "https://screenote.test",
		Project: "3",
		Login: &appconfig.LoginCredentials{
			AccessToken: "stored-access-token",
			ClientID:    "client-1",
			BaseURL:     "https://screenote.test",
		},
	}); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runCLI(t, []string{"--config", configPath, "config"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stdout, "stored-access-token") {
		t.Fatalf("stdout leaked token: %s", stdout)
	}
	var payload struct {
		TokenSet bool `json:"token_set"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout=%s err=%v", stdout, err)
	}
	if !payload.TokenSet {
		t.Fatalf("payload=%#v", payload)
	}

	stdout, stderr, code = runCLI(t, []string{
		"--config", configPath,
		"--base-url", "https://different-screenote.test",
		"config",
	}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout=%s err=%v", stdout, err)
	}
	if payload.TokenSet {
		t.Fatalf("stored login for a different base URL should not be reported as configured: %#v", payload)
	}
}

func TestScreenshotCreateDerivesContentType(t *testing.T) {
	contentTypes := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("project_id") != "7" {
			t.Fatalf("project_id=%q", r.FormValue("project_id"))
		}
		files := r.MultipartForm.File["image"]
		if len(files) == 0 {
			t.Fatal("no image part")
		}
		contentTypes <- files[0].Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"screenshot_id":9}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(path, []byte("png-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "screenshot", "create", "--title", "Home", "--file", path}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if got := <-contentTypes; got != "image/png" {
		t.Fatalf("content type=%q", got)
	}
}

func TestScreenshotCreateStdinContentTypeFallback(t *testing.T) {
	contentTypes := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("project_id") != "7" {
			t.Fatalf("project_id=%q", r.FormValue("project_id"))
		}
		files := r.MultipartForm.File["image"]
		if len(files) == 0 {
			t.Fatal("no image part")
		}
		contentTypes <- files[0].Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"screenshot_id":9}`))
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "screenshot", "create", "--title", "Home"}, "png-bytes")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if got := <-contentTypes; got != "application/octet-stream" {
		t.Fatalf("content type=%q", got)
	}
}

func TestCommentAddPathAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/annotations/5/comments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("body") != "hello" {
			t.Fatalf("body=%q", r.Form.Get("body"))
		}
		if r.Form.Get("project_id") != "7" {
			t.Fatalf("project_id=%q", r.Form.Get("project_id"))
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "hello"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

// aggregateServer serves the project-wide `annotation list` aggregate path:
// two pages of screenshots, per-screenshot annotation paging, and a
// configurable failure for one screenshot ID.
func aggregateServer(t *testing.T, failID int, failStatus int) *httptest.Server {
	t.Helper()
	// annotationCount maps a screenshot ID to its total annotation count.
	annotationCount := func(sid int) int {
		switch sid {
		case 1:
			return 150 // spans two annotation pages (100 + 50)
		default:
			return 1
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/projects/7/screenshots":
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			var ids []int
			if offset == 0 {
				for id := 1; id <= 100; id++ { // full page -> forces a second request
					ids = append(ids, id)
				}
			} else {
				ids = []int{101, 102}
			}
			parts := make([]string, 0, len(ids))
			for _, id := range ids {
				parts = append(parts, fmt.Sprintf(`{"id":%d}`, id))
			}
			fmt.Fprintf(w, `{"screenshots":[%s],"pagination":{"total":102,"limit":100,"offset":%d}}`, strings.Join(parts, ","), offset)
		case strings.HasPrefix(r.URL.Path, "/api/v1/screenshots/") && strings.HasSuffix(r.URL.Path, "/annotations"):
			if r.URL.Query().Get("project_id") != "7" {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			trimmed := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/screenshots/"), "/annotations")
			sid, err := strconv.Atoi(trimmed)
			if err != nil {
				t.Fatalf("bad screenshot id %q", trimmed)
			}
			if sid == failID {
				w.WriteHeader(failStatus)
				_, _ = w.Write([]byte(`{"error":"boom","code":"boom"}`))
				return
			}
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			total := annotationCount(sid)
			parts := make([]string, 0)
			for i := offset; i < total && i < offset+pageSize; i++ {
				parts = append(parts, fmt.Sprintf(`{"id":%d,"screenshot_id":%d}`, sid*1000+i, sid))
			}
			fmt.Fprintf(w, `{"annotations":[%s],"pagination":{"total":%d,"limit":%d,"offset":%d}}`, strings.Join(parts, ","), total, pageSize, offset)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
}

func TestAnnotationListAggregatesAcrossScreenshots(t *testing.T) {
	// Screenshot 2 is deleted between list and fetch (404) and must be
	// silently skipped: 150 (sid 1) + 0 (sid 2) + 100 (sid 3..102) = 250.
	server := aggregateServer(t, 2, http.StatusNotFound)
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "list", "--limit", "10", "--offset", "5"}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}

	var payload struct {
		Annotations []struct {
			ID int `json:"id"`
		} `json:"annotations"`
		Pagination struct {
			Total  int `json:"total"`
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout=%s err=%v", stdout, err)
	}
	if payload.Pagination.Total != 250 {
		t.Fatalf("total=%d want 250", payload.Pagination.Total)
	}
	if len(payload.Annotations) != 10 {
		t.Fatalf("window len=%d want 10", len(payload.Annotations))
	}
	if payload.Pagination.Limit != 10 || payload.Pagination.Offset != 5 {
		t.Fatalf("pagination=%+v", payload.Pagination)
	}
}

func TestAnnotationListPropagatesServerError(t *testing.T) {
	// A 500 from one screenshot must fail the whole listing rather than seal
	// a partial result as complete.
	server := aggregateServer(t, 2, http.StatusInternalServerError)
	defer server.Close()

	_, _, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "list"}, "")
	if code == ExitOK {
		t.Fatalf("expected non-zero exit for server error, got %d", code)
	}
}

func TestUnknownCommandExitsUsage(t *testing.T) {
	stdout, stderr, code := runCLI(t, []string{"--base-url", "http://example.test", "--token", "key", "bogus"}, "")
	if code != ExitUsage {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(stderr), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["code"] != "unexpected_arguments" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestUnknownSubcommandExitsUsage(t *testing.T) {
	_, _, code := runCLI(t, []string{"--base-url", "http://example.test", "--token", "key", "project", "bogus"}, "")
	if code != ExitUsage {
		t.Fatalf("code=%d", code)
	}
}

func TestStrayPositionalArgExitsUsage(t *testing.T) {
	_, _, code := runCLI(t, []string{"--base-url", "http://example.test", "--token", "key", "project", "list", "extra"}, "")
	if code != ExitUsage {
		t.Fatalf("code=%d", code)
	}
}

func cropPNG(t *testing.T) (string, []byte) {
	return cropPNGSize(t, 2, 2)
}

func cropPNGSize(t *testing.T, width, height int) (string, []byte) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 25, G: 50, B: 75, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(out.Bytes()), out.Bytes()
}
