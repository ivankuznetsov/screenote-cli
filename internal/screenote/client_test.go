package screenote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientSendsAuthAndParsesErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		http.Error(w, `{"error":"nope","code":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = client.Projects(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Code != "unauthorized" {
		t.Fatalf("apiErr = %#v", apiErr)
	}
}

func TestClientCreateProject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("Content-Type = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("name"); got != "Plugin review" {
			t.Fatalf("name = %q", got)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"project":{"id":7,"name":"Plugin review","role":"owner"}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.CreateProject(context.Background(), "Plugin review")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"role":"owner"`) {
		t.Fatalf("raw = %s", raw)
	}
}

func TestClientResolveAnnotation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/annotations/5/resolve" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("project_id"); got != "7" {
			t.Fatalf("project_id = %q", got)
		}
		if got := r.Form.Get("comment"); got != "Fixed in abc1234" {
			t.Fatalf("comment = %q", got)
		}
		_, _ = w.Write([]byte(`{"success":true,"operation":"resolved","annotation":{"id":5,"status":"resolved"}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.ResolveAnnotation(context.Background(), "5", "7", "Fixed in abc1234")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"operation":"resolved"`) {
		t.Fatalf("raw = %s", raw)
	}
}

func TestClientCreateProjectReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"Name cannot be blank","code":"invalid_name"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateProject(context.Background(), " ")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity || apiErr.Code != "invalid_name" {
		t.Fatalf("error = %#v", err)
	}
}

func TestClientCreateScreenshotMultipart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/screenshots" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("title"); got != "Home" {
			t.Fatalf("title = %q", got)
		}
		if got := r.FormValue("project_id"); got != "7" {
			t.Fatalf("project_id = %q", got)
		}
		file, header, err := r.FormFile("image")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if header.Filename != "stdin.png" || string(data) != "png-data" {
			t.Fatalf("file %q %q", header.Filename, string(data))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"screenshot_id": 7})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.CreateScreenshot(context.Background(), "7", "Home", "", "stdin.png", "image/png", strings.NewReader("png-data"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"screenshot_id":7`) {
		t.Fatalf("raw = %s", raw)
	}
}

func TestClientEmptyTokenOmitsAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"projects":[]}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Projects(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewClientUsesBoundedDefaultHTTPClient(t *testing.T) {
	client, err := NewClient("https://screenote.test", "token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient.Timeout != defaultHTTPTimeout {
		t.Fatalf("timeout=%s want %s", client.httpClient.Timeout, defaultHTTPTimeout)
	}
	if client.uploadHTTPClient.Timeout != defaultUploadHTTPTimeout {
		t.Fatalf("upload timeout=%s want %s", client.uploadHTTPClient.Timeout, defaultUploadHTTPTimeout)
	}
	if oauthClient := httpClientOrDefault(nil); oauthClient.Timeout != defaultHTTPTimeout {
		t.Fatalf("OAuth timeout=%s want %s", oauthClient.Timeout, defaultHTTPTimeout)
	}
}

func TestNewClientPreservesCustomHTTPClientForUploads(t *testing.T) {
	custom := &http.Client{Timeout: 17 * time.Second}
	client, err := NewClient("https://screenote.test", "token", custom)
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient != custom || client.uploadHTTPClient != custom {
		t.Fatal("custom HTTP client policy was not preserved for every request type")
	}
}

func TestClientSnapshotPrepareUploadAndShow(t *testing.T) {
	requests := make(chan *http.Request, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/7/snapshots":
			var request SnapshotPrepareRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ManifestDigest != "manifest" || len(request.Entries) != 1 || request.Entries[0].FileRefSHA256 != "ref" {
				t.Fatalf("request = %#v", request)
			}
			_ = json.NewEncoder(w).Encode(SnapshotResponse{
				Operation: "created", SnapshotID: 11, State: "awaiting_upload",
				Entries: []SnapshotEntryResponse{{ImageID: 12, Viewport: "desktop"}},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/projects/7/screenshot_images/12":
			if r.Header.Get("Content-Type") != "image/png" || r.ContentLength != 9 {
				t.Fatalf("headers = %#v length=%d", r.Header, r.ContentLength)
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != "png-bytes" {
				t.Fatalf("body = %q", body)
			}
			_ = json.NewEncoder(w).Encode(SnapshotImageUploadResponse{
				Operation: "uploaded", SnapshotID: 11, ImageID: 12,
				State: "processing", Status: "pending", Attached: true, SnapshotState: "processing",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/7/snapshots/11":
			_ = json.NewEncoder(w).Encode(SnapshotResponse{Operation: "status", SnapshotID: 11, State: "ready"})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	request := SnapshotPrepareRequest{
		Version: 1, GitCommit: "abc1234", TakenAt: "2026-07-10T10:00:00.000000Z", ManifestDigest: "manifest",
		Entries: []SnapshotPrepareEntry{{Page: "Home", Title: "Home", Viewport: "desktop", MIMEType: "image/png", ContentSHA256: "sha", FileRefSHA256: "ref"}},
	}
	prepared, err := client.PrepareSnapshot(context.Background(), "7", request)
	if err != nil || prepared.SnapshotID != 11 {
		t.Fatalf("prepared=%#v err=%v", prepared, err)
	}
	uploaded, err := client.UploadSnapshotImage(context.Background(), "7", 12, "image/png", 9, bytes.NewBufferString("png-bytes"))
	if err != nil || uploaded.Operation != "uploaded" {
		t.Fatalf("uploaded=%#v err=%v", uploaded, err)
	}
	shown, err := client.Snapshot(context.Background(), "7", 11)
	if err != nil || shown.State != "ready" {
		t.Fatalf("shown=%#v err=%v", shown, err)
	}

	for range 3 {
		req := <-requests
		if req.Header.Get("Authorization") != "Bearer token" || req.Header.Get("Accept") != "application/json" {
			t.Fatalf("headers = %#v", req.Header)
		}
	}
}
