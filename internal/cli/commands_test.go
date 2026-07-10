package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
