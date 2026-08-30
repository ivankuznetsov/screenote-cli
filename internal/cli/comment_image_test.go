package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
)

func TestCommentAddImagePostsOneSanitizedAttachment(t *testing.T) {
	_, image := cropPNG(t)
	secretPath := filepath.Join(t.TempDir(), "customer-secret-layout.png")
	if err := os.WriteFile(secretPath, image, 0o600); err != nil {
		t.Fatal(err)
	}

	var legacyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			legacyCalls.Add(1)
			t.Fatal("image mode called the legacy comments route")
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/annotations/5/image_comments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(maxCommentImageBytes + 1024); err != nil {
			t.Fatal(err)
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("project_id") != "7" || r.FormValue("body") != "change this element" {
			t.Fatalf("form = %#v", r.Form)
		}
		files := r.MultipartForm.File["images[]"]
		if len(files) != 1 || files[0].Filename != "attachment.png" || files[0].Header.Get("Content-Type") != "image/png" {
			t.Fatalf("files = %#v", files)
		}
		file, err := files[0].Open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil || !bytes.Equal(got, image) {
			t.Fatalf("image differs: bytes=%d err=%v", len(got), err)
		}
		digest := sha256.Sum256(image)
		if r.Header.Get("Screenote-Image-SHA256") != hex.EncodeToString(digest[:]) {
			t.Fatalf("digest = %q", r.Header.Get("Screenote-Image-SHA256"))
		}
		if key := r.Header.Get("Idempotency-Key"); !regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`).MatchString(key) {
			t.Fatalf("key = %q", key)
		}
		w.Header().Set("Screenote-API-Capability", screenote.ImageCommentsCapability)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"operation":"created","comment":{"id":11},"attachment":{"id":12,"media_type":"image/png"}}`))
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "change this element", "--image", secretPath}, "")
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, `"operation":"created"`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, secretPath) || legacyCalls.Load() != 0 {
		t.Fatalf("path leaked or legacy route called: stdout=%q stderr=%q calls=%d", stdout, stderr, legacyCalls.Load())
	}
}

func TestCommentAddImageAcceptsStdin(t *testing.T) {
	_, image := cropPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(maxCommentImageBytes + 1024); err != nil {
			t.Fatal(err)
		}
		defer r.MultipartForm.RemoveAll()
		file, _, err := r.FormFile("images[]")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(file)
		_ = file.Close()
		if !bytes.Equal(got, image) {
			t.Fatal("stdin image differs")
		}
		w.Header().Set("Screenote-API-Capability", screenote.ImageCommentsCapability)
		_, _ = w.Write([]byte(`{"operation":"replayed","comment":{"id":11},"attachment":{"id":12}}`))
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "body", "--image", "-"}, string(image))
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, `"operation":"replayed"`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

func TestCommentAddImageRejectsRepeatedFlagBeforeClientCreation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "body", "--image", "/secret/one.png", "--image", "/secret/two.png"}, "")
	if code != ExitUsage || !strings.Contains(stderr, `"code":"invalid_image"`) || calls.Load() != 0 {
		t.Fatalf("code=%d stderr=%s calls=%d", code, stderr, calls.Load())
	}
	if strings.Contains(stderr, "/secret/") {
		t.Fatalf("source path leaked: %s", stderr)
	}
}

func TestCommentAddImageRejectsLocalInputWithoutNetworkOrPathLeak(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()

	tests := []struct {
		name string
		data []byte
		code string
	}{
		{name: "empty", code: "empty_image"},
		{name: "unsupported", data: []byte("not an image"), code: "unsupported_image_type"},
		{name: "oversized", data: append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, maxCommentImageBytes-7)...), code: "image_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			secretPath := filepath.Join(t.TempDir(), "never-print-this.png")
			if err := os.WriteFile(secretPath, test.data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, stderr, exit := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "body", "--image", secretPath}, "")
			if exit == ExitOK || !strings.Contains(stderr, `"code":"`+test.code+`"`) {
				t.Fatalf("exit=%d stderr=%s", exit, stderr)
			}
			if strings.Contains(stderr, secretPath) {
				t.Fatalf("path leaked: %s", stderr)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("network calls = %d", calls.Load())
	}
}

func TestCommentAddImageRejectsDirectoryWithoutPathLeak(t *testing.T) {
	directory := t.TempDir()
	_, stderr, code := runCLI(t, []string{"--base-url", "https://screenote.test", "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "body", "--image", directory}, "")
	if code != ExitUsage || !strings.Contains(stderr, `"code":"image_read_failed"`) {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stderr, directory) {
		t.Fatalf("path leaked: %s", stderr)
	}
}

func TestSpoolCommentImageAcceptsAllowedSignaturesAndExactLimit(t *testing.T) {
	tests := []struct {
		name        string
		data        []byte
		contentType string
		filename    string
	}{
		{name: "png", data: []byte("\x89PNG\r\n\x1a\nbytes"), contentType: "image/png", filename: "attachment.png"},
		{name: "jpeg", data: []byte("\xff\xd8\xff\xe0bytes"), contentType: "image/jpeg", filename: "attachment.jpg"},
		{name: "webp", data: []byte("RIFF\x04\x00\x00\x00WEBPbytes"), contentType: "image/webp", filename: "attachment.webp"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			image, err := spoolCommentImage(bytes.NewReader(test.data), "-")
			if err != nil {
				t.Fatal(err)
			}
			defer image.cleanup()
			if image.contentType != test.contentType || image.filename != test.filename {
				t.Fatalf("image = %#v", image)
			}
		})
	}

	prefix := []byte("\x89PNG\r\n\x1a\n")
	exact := io.MultiReader(bytes.NewReader(prefix), io.LimitReader(commentZeroReader{}, maxCommentImageBytes-int64(len(prefix))))
	image, err := spoolCommentImage(exact, "-")
	if err != nil {
		t.Fatal(err)
	}
	defer image.cleanup()
	info, err := os.Stat(image.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != maxCommentImageBytes || info.Mode().Perm() != 0o600 {
		t.Fatalf("size=%d mode=%#o", info.Size(), info.Mode().Perm())
	}
}

func TestCommentAddImageRetriesOnceWithSameKeyAndPayload(t *testing.T) {
	_, image := cropPNG(t)
	imagePath := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(imagePath, image, 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var keys, bodies []string
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		requests++
		attempt := requests
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		bodies = append(bodies, string(raw))
		mu.Unlock()
		if attempt == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server cannot hijack")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Screenote-API-Capability", screenote.ImageCommentsCapability)
		_, _ = w.Write([]byte(`{"operation":"replayed","comment":{"id":11},"attachment":{"id":12}}`))
	}))
	defer server.Close()

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "body", "--image", imagePath}, "")
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, `"operation":"replayed"`) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] || len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("keys=%q body_equal=%t", keys, len(bodies) == 2 && bodies[0] == bodies[1])
	}
}

func TestCommentAddImageReportsUnknownAfterTwoAmbiguousResponses(t *testing.T) {
	_, image := cropPNG(t)
	var requests atomic.Int32
	var legacy atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			legacy.Add(1)
		}
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		hijacker := w.(http.Hijacker)
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "body", "--image", "-"}, string(image))
	if code != ExitGeneric || !strings.Contains(stderr, `"code":"comment_result_unknown"`) || requests.Load() != 2 || legacy.Load() != 0 {
		t.Fatalf("code=%d stderr=%s requests=%d legacy=%d", code, stderr, requests.Load(), legacy.Load())
	}
}

func TestCommentAddImageClassifiesMissingCapabilityOnly(t *testing.T) {
	_, image := cropPNG(t)
	tests := []struct {
		name       string
		capability bool
		body       string
		wantCode   string
		wantExit   int
	}{
		{name: "old server", body: `{"error":"Not found","code":"not_found"}`, wantCode: "image_comments_unsupported", wantExit: ExitGeneric},
		{name: "new server annotation missing", capability: true, body: `{"error":"Annotation not found","code":"annotation_not_found"}`, wantCode: "annotation_not_found", wantExit: ExitNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.capability {
					w.Header().Set("Screenote-API-Capability", screenote.ImageCommentsCapability)
				}
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "body", "--image", "-"}, string(image))
			if code != test.wantExit || !strings.Contains(stderr, `"code":"`+test.wantCode+`"`) {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
		})
	}
}

func TestCommentAddImageDoesNotRetryCanceledContext(t *testing.T) {
	_, image := cropPNG(t)
	var calls atomic.Int32
	client := &http.Client{Transport: commentRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stdout, stderr, code := runCLIWithContext(t, ctx, client, []string{"--base-url", "https://screenote.test", "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "body", "--image", "-"}, string(image))
	if stdout != "" || code != ExitGeneric || !strings.Contains(stderr, `"code":"request_canceled"`) || calls.Load() > 1 {
		t.Fatalf("code=%d stdout=%s stderr=%s calls=%d", code, stdout, stderr, calls.Load())
	}
}

func TestCommentAddImageGatewayRetriesOnce(t *testing.T) {
	_, image := cropPNG(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"Bad gateway","code":"http_502"}`))
			return
		}
		w.Header().Set("Screenote-API-Capability", screenote.ImageCommentsCapability)
		_, _ = w.Write([]byte(`{"operation":"replayed","comment":{"id":11},"attachment":{"id":12}}`))
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "comment", "add", "--annotation", "5", "--body", "body", "--image", "-"}, string(image))
	if code != ExitOK || stderr != "" || calls.Load() != 2 {
		t.Fatalf("code=%d stderr=%s calls=%d", code, stderr, calls.Load())
	}
}

type commentZeroReader struct{}

func (commentZeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func runCLIWithContext(t *testing.T, ctx context.Context, client *http.Client, args []string, stdin string) (string, string, int) {
	t.Helper()
	args = append([]string{"--config", filepath.Join(t.TempDir(), "config.toml")}, args...)
	var stdout, stderr bytes.Buffer
	command := NewTestCommand(ctx, strings.NewReader(stdin), &stdout, &stderr, client)
	command.SetArgs(args)
	if err := command.ExecuteContext(ctx); err != nil {
		code := writeError(&stderr, err)
		return stdout.String(), stderr.String(), code
	}
	return stdout.String(), stderr.String(), ExitOK
}

type commentRoundTripFunc func(*http.Request) (*http.Response, error)

func (f commentRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func decodeJSONError(t *testing.T, raw string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("invalid JSON error %q: %v", raw, err)
	}
	return payload
}
