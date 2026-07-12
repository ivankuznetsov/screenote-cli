package cli

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
	snapshotmanifest "github.com/ivankuznetsov/screenote-cli/internal/snapshot"
)

func TestSnapshotCommandPublishesManifestAndWaitsForReady(t *testing.T) {
	manifestPath, prepared := createSnapshotManifest(t, 2)
	var mu sync.Mutex
	var uploads []int
	var prepareBody string
	showCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/7/snapshots":
			var request screenote.SnapshotPrepareRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(request)
			prepareBody = string(encoded)
			_ = json.NewEncoder(w).Encode(snapshotResponseFor(prepared, "created", "awaiting_upload", false))
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/screenshot_images/"):
			imageID, err := strconv.Atoi(filepath.Base(r.URL.Path))
			if err != nil {
				t.Fatal(err)
			}
			body := new(bytes.Buffer)
			_, _ = body.ReadFrom(r.Body)
			if body.Len() == 0 || r.Header.Get("Content-Type") != "image/png" {
				t.Fatalf("upload body=%d type=%q", body.Len(), r.Header.Get("Content-Type"))
			}
			mu.Lock()
			uploads = append(uploads, imageID)
			mu.Unlock()
			snapshotState := "awaiting_upload"
			if imageID == 101 {
				snapshotState = "processing"
			}
			_ = json.NewEncoder(w).Encode(screenote.SnapshotImageUploadResponse{
				Operation: "uploaded", SnapshotID: 41, ImageID: imageID,
				State: "processing", Status: "pending", Attached: true, SnapshotState: snapshotState,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/7/snapshots/41":
			showCalls++
			state := "processing"
			if showCalls > 1 {
				state = "ready"
			}
			response := snapshotResponseFor(prepared, "status", state, true)
			response.ReviewURL = "https://screenote.test/projects/7?snapshot_id=41"
			_ = json.NewEncoder(w).Encode(response)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{
		"--base-url", server.URL, "--token", "super-secret-token", "--project", "7",
		"snapshot", "--manifest", manifestPath, "--wait", "1s",
	}, "")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s stdout=%s", code, stderr, stdout)
	}
	if len(uploads) != 2 || showCalls < 2 {
		t.Fatalf("uploads=%v showCalls=%d", uploads, showCalls)
	}
	if strings.Contains(prepareBody, filepath.Dir(manifestPath)) || strings.Contains(prepareBody, `"file"`) {
		t.Fatalf("prepare request leaked local paths: %s", prepareBody)
	}
	events := decodeJSONLines(t, stdout)
	wantSequence := []string{"snapshot_prepared", "image_uploaded", "image_uploaded", "snapshot_state", "snapshot_state", "snapshot_ready"}
	if len(events) != len(wantSequence) {
		t.Fatalf("event count=%d want=%d events=%#v", len(events), len(wantSequence), events)
	}
	for index, want := range wantSequence {
		if events[index]["event"] != want {
			t.Fatalf("event %d=%#v want=%s", index, events[index], want)
		}
	}
	assertJSONEventShape(t, events[0], map[string]string{
		"event": "string", "operation": "string", "snapshot_id": "number", "manifest_digest": "string", "state": "string",
	})
	for _, event := range events[1:3] {
		assertJSONEventShape(t, event, map[string]string{
			"event": "string", "operation": "string", "snapshot_id": "number", "manifest_entry": "number",
			"image_id": "number", "viewport": "string", "state": "string",
		})
	}
	for _, event := range events[3:5] {
		assertJSONEventShape(t, event, map[string]string{"event": "string", "snapshot_id": "number", "state": "string"})
	}
	assertJSONEventShape(t, events[5], map[string]string{
		"event": "string", "snapshot_id": "number", "state": "string", "review_url": "string",
	})
	if events[0]["operation"] != "created" || events[1]["manifest_entry"] != float64(0) || events[2]["manifest_entry"] != float64(1) {
		t.Fatalf("events=%#v", events)
	}
	if events[len(events)-1]["review_url"] != "https://screenote.test/projects/7?snapshot_id=41" {
		t.Fatalf("final=%#v", events[len(events)-1])
	}
	if strings.Contains(stdout, filepath.Dir(manifestPath)) || strings.Contains(stdout, "super-secret-token") || strings.Contains(stderr, "super-secret-token") {
		t.Fatalf("output leaked a local path or bearer token: stdout=%s stderr=%s", stdout, stderr)
	}
}

func TestSnapshotCommandResumesAndSkipsAttachedImages(t *testing.T) {
	manifestPath, prepared := createSnapshotManifest(t, 2)
	var uploads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(snapshotResponseFor(prepared, "resumed", "awaiting_upload", true))
		case http.MethodPut:
			uploads++
			_ = json.NewEncoder(w).Encode(screenote.SnapshotImageUploadResponse{
				Operation: "uploaded", SnapshotID: 41, ImageID: 101,
				State: "processing", Status: "pending", Attached: true, SnapshotState: "processing",
			})
		case http.MethodGet:
			response := snapshotResponseFor(prepared, "status", "ready", true)
			response.ReviewURL = "https://screenote.test/review"
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{
		"--base-url", server.URL, "--token", "key", "--project", "7",
		"snapshot", "--manifest", manifestPath, "--wait", "1s",
	}, "")
	if code != ExitOK || uploads != 1 {
		t.Fatalf("code=%d uploads=%d stderr=%s", code, uploads, stderr)
	}
	if !strings.Contains(stdout, `"event":"image_skipped"`) || !strings.Contains(stdout, `"operation":"resumed"`) {
		t.Fatalf("stdout=%s", stdout)
	}
	events := decodeJSONLines(t, stdout)
	wantSequence := []string{"snapshot_prepared", "image_skipped", "image_uploaded", "snapshot_state", "snapshot_state", "snapshot_ready"}
	if len(events) != len(wantSequence) {
		t.Fatalf("events=%#v", events)
	}
	for index, want := range wantSequence {
		if events[index]["event"] != want {
			t.Fatalf("event %d=%#v want=%s", index, events[index], want)
		}
	}
	assertJSONEventShape(t, events[1], map[string]string{
		"event": "string", "operation": "string", "snapshot_id": "number", "manifest_entry": "number",
		"image_id": "number", "viewport": "string",
	})
}

func TestSnapshotCommandRetriesAttachedFailedImage(t *testing.T) {
	manifestPath, prepared := createSnapshotManifest(t, 1)
	putCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			response := snapshotResponseFor(prepared, "resumed", "failed", true)
			response.Entries[0].Status = "failed"
			_ = json.NewEncoder(w).Encode(response)
		case http.MethodPut:
			putCalls++
			_ = json.NewEncoder(w).Encode(screenote.SnapshotImageUploadResponse{
				Operation: "processing_retried", SnapshotID: 41, ImageID: 100,
				State: "processing", Status: "pending", Attached: true, SnapshotState: "processing",
			})
		case http.MethodGet:
			response := snapshotResponseFor(prepared, "status", "ready", true)
			response.ReviewURL = "https://screenote.test/recovered"
			_ = json.NewEncoder(w).Encode(response)
		default:
			t.Fatalf("unexpected request %s", r.Method)
		}
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{
		"--base-url", server.URL, "--token", "key", "--project", "7",
		"snapshot", "--manifest", manifestPath, "--wait", "1s",
	}, "")
	if code != ExitOK || putCalls != 1 {
		t.Fatalf("code=%d putCalls=%d stderr=%s stdout=%s", code, putCalls, stderr, stdout)
	}
	if !strings.Contains(stdout, `"operation":"processing_retried"`) || !strings.Contains(stdout, `"event":"snapshot_ready"`) {
		t.Fatalf("stdout=%s", stdout)
	}
}

func TestSnapshotCommandRetriesTransientPollFailure(t *testing.T) {
	manifestPath, prepared := createSnapshotManifest(t, 1)
	getCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(snapshotResponseFor(prepared, "resumed", "processing", true))
		case http.MethodGet:
			getCalls++
			if getCalls == 1 {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"code":"upstream_unavailable","error":"temporary failure"}`))
				return
			}
			response := snapshotResponseFor(prepared, "status", "ready", true)
			response.ReviewURL = "https://screenote.test/recovered"
			_ = json.NewEncoder(w).Encode(response)
		default:
			t.Fatalf("unexpected request %s", r.Method)
		}
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{
		"--base-url", server.URL, "--token", "key", "--project", "7",
		"snapshot", "--manifest", manifestPath, "--wait", "1s",
	}, "")
	if code != ExitOK || getCalls != 2 || !strings.Contains(stdout, `"event":"snapshot_ready"`) {
		t.Fatalf("code=%d getCalls=%d stderr=%s stdout=%s", code, getCalls, stderr, stdout)
	}
}

func TestSnapshotCommandPreflightFailsBeforeNetwork(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(manifestPath, []byte(`{"version":1,"git_commit":"abc1234","taken_at":"2026-07-10T10:00:00Z","images":[{"page":"Home","file":"missing.png","viewport":"desktop"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{
		"--base-url", server.URL, "--token", "key", "--project", "7",
		"snapshot", "--manifest", manifestPath,
	}, "")
	if code != ExitUsage || called || stdout != "" {
		t.Fatalf("code=%d called=%v stdout=%q stderr=%s", code, called, stdout, stderr)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stderr), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["code"] != "image_unreadable" || payload["operation"] != "preflight" || strings.Contains(stderr, dir) {
		t.Fatalf("payload=%#v stderr=%s", payload, stderr)
	}
}

func TestSnapshotCommandStopsAtFirstUploadFailureWithContext(t *testing.T) {
	manifestPath, prepared := createSnapshotManifest(t, 2)
	putCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(snapshotResponseFor(prepared, "created", "awaiting_upload", false))
		case http.MethodPut:
			putCalls++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"upload_failed","error":"temporary failure"}`))
		default:
			t.Fatalf("unexpected request %s", r.Method)
		}
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{
		"--base-url", server.URL, "--token", "key", "--project", "7",
		"snapshot", "--manifest", manifestPath, "--wait", "1s",
	}, "")
	if code != ExitGeneric || putCalls != 1 {
		t.Fatalf("code=%d putCalls=%d stderr=%s", code, putCalls, stderr)
	}
	if !strings.Contains(stdout, `"event":"snapshot_prepared"`) {
		t.Fatalf("stdout=%s", stdout)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stderr), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["operation"] != "upload_image" || payload["manifest_entry"] != float64(0) || payload["snapshot_id"] != float64(41) {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestSnapshotCommandRejectsMismatchedUploadResponse(t *testing.T) {
	manifestPath, prepared := createSnapshotManifest(t, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(snapshotResponseFor(prepared, "created", "awaiting_upload", false))
		case http.MethodPut:
			_ = json.NewEncoder(w).Encode(screenote.SnapshotImageUploadResponse{
				Operation: "uploaded", SnapshotID: 42, ImageID: 100,
				State: "processing", Status: "pending", Attached: true, SnapshotState: "processing",
			})
		default:
			t.Fatalf("unexpected request %s", r.Method)
		}
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{
		"--base-url", server.URL, "--token", "key", "--project", "7",
		"snapshot", "--manifest", manifestPath, "--wait", "1s",
	}, "")
	if code != ExitGeneric || !strings.Contains(stderr, `"code":"invalid_response"`) || !strings.Contains(stderr, `"operation":"upload_image"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestSnapshotCommandTimeoutIsResumableFailure(t *testing.T) {
	manifestPath, prepared := createSnapshotManifest(t, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(snapshotResponseFor(prepared, "resumed", "processing", true))
			return
		}
		_ = json.NewEncoder(w).Encode(snapshotResponseFor(prepared, "status", "processing", true))
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{
		"--base-url", server.URL, "--token", "key", "--project", "7",
		"snapshot", "--manifest", manifestPath, "--wait", "20ms",
	}, "")
	if code != ExitGeneric || !strings.Contains(stderr, `"code":"snapshot_timeout"`) || !strings.Contains(stderr, `"snapshot_id":41`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestSnapshotPollBackoffIsCapped(t *testing.T) {
	interval := defaultPollInterval
	want := []time.Duration{2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for index, expected := range want {
		interval = nextSnapshotPollInterval(interval)
		if interval != expected {
			t.Fatalf("step %d interval=%s want=%s", index, interval, expected)
		}
	}
}

func TestRetryableSnapshotPollErrorClassifiesHTTPStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{status: http.StatusTooManyRequests, want: true},
		{status: http.StatusBadGateway, want: true},
		{status: http.StatusServiceUnavailable, want: true},
		{status: http.StatusNotFound, want: false},
		{status: http.StatusUnauthorized, want: false},
	}
	for _, test := range cases {
		err := &screenote.Error{StatusCode: test.status}
		if got := retryableSnapshotPollError(err); got != test.want {
			t.Fatalf("status=%d retryable=%v want=%v", test.status, got, test.want)
		}
	}
}

func createSnapshotManifest(t *testing.T, count int) (string, *snapshotmanifest.PreparedManifest) {
	t.Helper()
	dir := t.TempDir()
	images := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		name := "image-" + string(rune('a'+i)) + ".png"
		path := filepath.Join(dir, name)
		writeCLITestPNG(t, path, color.RGBA{R: uint8(i + 1), A: 255})
		images = append(images, map[string]any{
			"page": "Page " + string(rune('A'+i)), "file": name, "viewport": "desktop",
		})
	}
	manifest := map[string]any{
		"version": 1, "git_commit": "abc1234", "taken_at": "2026-07-10T10:00:00Z", "images": images,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := snapshotmanifest.Load(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	return manifestPath, prepared
}

func snapshotResponseFor(prepared *snapshotmanifest.PreparedManifest, operation, state string, attachedFirst bool) screenote.SnapshotResponse {
	entries := make([]screenote.SnapshotEntryResponse, 0, len(prepared.Images))
	for index, image := range prepared.Images {
		attached := attachedFirst && index == 0
		entries = append(entries, screenote.SnapshotEntryResponse{
			ScreenshotID: 200 + index, ManifestEntryDigest: image.GroupDigest,
			PageID: 300 + index, Page: image.Page, Title: image.Title,
			ImageID: 100 + index, Viewport: image.Viewport, MIMEType: image.MIMEType,
			ContentSHA256: image.ContentSHA256, State: state, Status: "pending", Attached: attached,
		})
	}
	return screenote.SnapshotResponse{
		Operation: operation, SnapshotID: 41, ProjectID: 7, ManifestDigest: prepared.ManifestDigest,
		GitCommit: prepared.GitCommit, TakenAt: prepared.TakenAt, State: state, Entries: entries,
	}
}

func writeCLITestPNG(t *testing.T, path string, c color.Color) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, c)
		}
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
}

func decodeJSONLines(t *testing.T, output string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	events := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line=%q err=%v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func assertJSONEventShape(t *testing.T, event map[string]any, expected map[string]string) {
	t.Helper()
	if len(event) != len(expected) {
		t.Fatalf("event keys=%v want=%v event=%#v", event, expected, event)
	}
	for key, typeName := range expected {
		value, ok := event[key]
		if !ok {
			t.Fatalf("event is missing %q: %#v", key, event)
		}
		switch typeName {
		case "string":
			if _, ok := value.(string); !ok {
				t.Fatalf("event[%q]=%T want string", key, value)
			}
		case "number":
			if _, ok := value.(float64); !ok {
				t.Fatalf("event[%q]=%T want number", key, value)
			}
		default:
			t.Fatalf("unknown expected type %q", typeName)
		}
	}
}
