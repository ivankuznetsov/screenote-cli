package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAnnotationGetDownloadsRootAndReplyAttachmentsPrivately(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nroot")
	jpeg := []byte("\xff\xd8\xffreply")
	webp := []byte("RIFF\x04\x00\x00\x00WEBPwebp")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/annotations/5":
			writeAttachmentDetail(t, w, map[string]any{
				"id": 5, "future_root": map[string]any{"kept": true},
				"attachments": []any{
					attachmentJSON(server.URL, 12, "image/png", png, "root-token", time.Now().Add(5*time.Minute)),
					attachmentJSON(server.URL, 14, "image/webp", webp, "webp-token", time.Now().Add(5*time.Minute)),
				},
				"comments": []any{map[string]any{
					"id": 20, "body": "reply", "future_reply": []any{"kept"},
					"attachments": []any{attachmentJSON(server.URL, 13, "image/jpeg", jpeg, "reply-token", time.Now().Add(5*time.Minute))},
				}},
			})
		case "/api/media/image_attachments/12":
			assertAttachmentRequest(t, r, "root-token")
			writeImage(w, "image/png", png)
		case "/api/media/image_attachments/13":
			assertAttachmentRequest(t, r, "reply-token")
			writeImage(w, "image/jpeg", jpeg)
		case "/api/media/image_attachments/14":
			assertAttachmentRequest(t, r, "webp-token")
			writeImage(w, "image/webp", webp)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	directory := filepath.Join(t.TempDir(), "attachments")
	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", directory}, "")
	if code != ExitOK || stderr != "" {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	assertPrivateFile(t, filepath.Join(directory, "attachment-12.png"), png)
	assertPrivateFile(t, filepath.Join(directory, "attachment-13.jpg"), jpeg)
	assertPrivateFile(t, filepath.Join(directory, "attachment-14.webp"), webp)
	if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode=%#o err=%v", info.Mode().Perm(), err)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("stdout=%s err=%v", stdout, err)
	}
	if payload["future_root"].(map[string]any)["kept"] != true {
		t.Fatalf("future root field lost: %#v", payload)
	}
	root := payload["attachments"].([]any)[0].(map[string]any)
	reply := payload["comments"].([]any)[0].(map[string]any)
	replyAttachment := reply["attachments"].([]any)[0].(map[string]any)
	for _, attachment := range []map[string]any{root, replyAttachment} {
		if _, exists := attachment["url"]; exists {
			t.Fatalf("URL was not redacted: %#v", attachment)
		}
		if _, exists := attachment["url_expires_at"]; exists {
			t.Fatalf("expiry was not redacted: %#v", attachment)
		}
		if !filepath.IsAbs(attachment["local_path"].(string)) {
			t.Fatalf("local path is not absolute: %#v", attachment)
		}
	}
	if reply["future_reply"].([]any)[0] != "kept" {
		t.Fatalf("future reply field lost: %#v", reply)
	}
}

func TestAnnotationGetRejectsCropAttachmentCollisionBeforeMedia(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nroot")
	encoded, _ := cropPNG(t)
	var mediaCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/5" {
			writeAttachmentDetail(t, w, map[string]any{
				"id": 5, "cropped_image_base64": encoded,
				"attachments": []any{attachmentJSON(server.URL, 12, "image/png", png, "token", time.Now().Add(5*time.Minute))},
				"comments":    []any{},
			})
			return
		}
		mediaCalls.Add(1)
	}))
	defer server.Close()
	directory := t.TempDir()
	cropPath := filepath.Join(directory, "attachment-12.png")

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", directory, "--crop-file", cropPath}, "")
	if code != ExitUsage || !strings.Contains(stderr, `"code":"output_collision"`) || mediaCalls.Load() != 0 {
		t.Fatalf("code=%d stderr=%s media=%d", code, stderr, mediaCalls.Load())
	}
}

func TestAnnotationGetAttachmentModeCreatesEmptyPrivateDirectoryWithoutMediaRequests(t *testing.T) {
	var mediaCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/annotations/5" {
			mediaCalls.Add(1)
			t.Fatal("unexpected media request")
		}
		_, _ = w.Write([]byte(`{"id":5,"unknown":true,"attachments":[],"comments":[{"id":20,"attachments":[]}]}`))
	}))
	defer server.Close()
	directory := filepath.Join(t.TempDir(), "empty")

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", directory}, "")
	if code != ExitOK || stderr != "" || mediaCalls.Load() != 0 || !strings.Contains(stdout, `"unknown":true`) {
		t.Fatalf("code=%d stdout=%s stderr=%s media=%d", code, stdout, stderr, mediaCalls.Load())
	}
	if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode=%#o err=%v", info.Mode().Perm(), err)
	}
}

func TestAnnotationGetAttachmentModeAcceptsSafeExistingDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":5,"attachments":[],"comments":[]}`))
	}))
	defer server.Close()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", directory}, "")
	if code != ExitOK || stderr != "" {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestAnnotationGetAttachmentModeRejectsEmptyFlagBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir="}, "")
	if code != ExitUsage || !strings.Contains(stderr, `"code":"invalid_attachments_directory"`) || calls.Load() != 0 {
		t.Fatalf("code=%d stderr=%s calls=%d", code, stderr, calls.Load())
	}
}

func TestAnnotationGetAttachmentModeRequiresRootAndReplyContractKeys(t *testing.T) {
	tests := []string{
		`{"id":5,"comments":[]}`,
		`{"id":5,"attachments":[],"comments":[{"id":20}]}`,
	}
	for index, response := range tests {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(response)) }))
			defer server.Close()
			_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", filepath.Join(t.TempDir(), "attachments")}, "")
			if code != ExitGeneric || !strings.Contains(stderr, `"code":"attachments_unsupported"`) {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
		})
	}
}

func TestAnnotationGetAttachmentPreflightRejectsUnsafeTargetsBeforeMedia(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nroot")
	var mediaCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/5" {
			writeAttachmentDetail(t, w, map[string]any{
				"id":          5,
				"attachments": []any{attachmentJSON(server.URL, 12, "image/png", png, "token", time.Now().Add(5*time.Minute))},
				"comments":    []any{},
			})
			return
		}
		mediaCalls.Add(1)
	}))
	defer server.Close()

	t.Run("existing target", func(t *testing.T) {
		directory := t.TempDir()
		target := filepath.Join(directory, "attachment-12.png")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", directory}, "")
		if code != ExitUsage || !strings.Contains(stderr, `"code":"attachment_exists"`) {
			t.Fatalf("code=%d stderr=%s", code, stderr)
		}
		if contents, _ := os.ReadFile(target); string(contents) != "keep" {
			t.Fatalf("target changed: %q", contents)
		}
	})

	t.Run("world writable directory", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o777); err != nil {
			t.Fatal(err)
		}
		_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", directory}, "")
		if code != ExitUsage || !strings.Contains(stderr, `"code":"invalid_attachments_directory"`) {
			t.Fatalf("code=%d stderr=%s", code, stderr)
		}
	})

	t.Run("symlink directory", func(t *testing.T) {
		realDirectory := t.TempDir()
		linkedDirectory := filepath.Join(t.TempDir(), "linked")
		if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", linkedDirectory}, "")
		if code != ExitUsage || !strings.Contains(stderr, `"code":"invalid_attachments_directory"`) {
			t.Fatalf("code=%d stderr=%s", code, stderr)
		}
	})

	if mediaCalls.Load() != 0 {
		t.Fatalf("media requests = %d", mediaCalls.Load())
	}
}

func TestAnnotationGetRejectsDuplicateAttachmentIDsBeforeMedia(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nroot")
	var mediaCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/5" {
			attachment := attachmentJSON(server.URL, 12, "image/png", png, "token", time.Now().Add(5*time.Minute))
			writeAttachmentDetail(t, w, map[string]any{
				"id": 5, "attachments": []any{attachment},
				"comments": []any{map[string]any{"id": 20, "attachments": []any{attachment}}},
			})
			return
		}
		mediaCalls.Add(1)
	}))
	defer server.Close()
	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", filepath.Join(t.TempDir(), "attachments")}, "")
	if code != ExitGeneric || !strings.Contains(stderr, `"code":"invalid_attachment_metadata"`) || mediaCalls.Load() != 0 {
		t.Fatalf("code=%d stderr=%s media=%d", code, stderr, mediaCalls.Load())
	}
}

func TestAnnotationGetRejectsCrossOriginAttachmentBeforeBearerLeak(t *testing.T) {
	var sinkCalls atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Fatal("bearer leaked to another origin")
		}
	}))
	defer sink.Close()
	png := []byte("\x89PNG\r\n\x1a\nroot")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeAttachmentDetail(t, w, map[string]any{
			"id":          5,
			"attachments": []any{attachmentJSON(sink.URL, 12, "image/png", png, "steal", time.Now().Add(5*time.Minute))},
			"comments":    []any{},
		})
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", filepath.Join(t.TempDir(), "attachments")}, "")
	if code != ExitGeneric || !strings.Contains(stderr, `"code":"invalid_attachment_url"`) || sinkCalls.Load() != 0 || strings.Contains(stderr, "steal") {
		t.Fatalf("code=%d stderr=%s sink=%d", code, stderr, sinkCalls.Load())
	}
}

func TestAnnotationGetAttachmentDownloadFailurePublishesNothingAndRedactsToken(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfirst")
	jpeg := []byte("\xff\xd8\xffsecond")
	const secretToken = "never-print-purpose-token"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/annotations/5":
			writeAttachmentDetail(t, w, map[string]any{
				"id": 5,
				"attachments": []any{
					attachmentJSON(server.URL, 12, "image/png", png, "first-token", time.Now().Add(5*time.Minute)),
					attachmentJSON(server.URL, 13, "image/jpeg", jpeg, secretToken, time.Now().Add(5*time.Minute)),
				},
				"comments": []any{},
			})
		case "/api/media/image_attachments/12":
			writeImage(w, "image/png", png)
		case "/api/media/image_attachments/13":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"provider failed for ` + secretToken + `","code":"provider_error"}`))
		}
	}))
	defer server.Close()
	directory := filepath.Join(t.TempDir(), "attachments")

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", directory}, "")
	if stdout != "" || code != ExitGeneric || !strings.Contains(stderr, `"code":"attachment_download_failed"`) || strings.Contains(stderr, secretToken) {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial files published: %#v", entries)
	}
}

func TestAnnotationGetRevokedAttachmentAccessPublishesNothingAndRedactsErrors(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfirst")
	jpeg := []byte("\xff\xd8\xffsecond")
	const purposeToken = "never-print-revoked-purpose-token"
	const bearerToken = "never-print-revoked-bearer"

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			codeName := "unauthorized"
			if status == http.StatusForbidden {
				codeName = "forbidden"
			}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/annotations/5":
					writeAttachmentDetail(t, w, map[string]any{
						"id": 5,
						"attachments": []any{
							attachmentJSON(server.URL, 12, "image/png", png, "first-token", time.Now().Add(5*time.Minute)),
							attachmentJSON(server.URL, 13, "image/jpeg", jpeg, purposeToken, time.Now().Add(5*time.Minute)),
						},
						"comments": []any{},
					})
				case "/api/media/image_attachments/12":
					writeImage(w, "image/png", png)
				case "/api/media/image_attachments/13":
					w.WriteHeader(status)
					_, _ = fmt.Fprintf(w, `{"error":"denied %s %s","code":"%s"}`, purposeToken, bearerToken, codeName)
				}
			}))
			defer server.Close()
			directory := filepath.Join(t.TempDir(), "attachments")

			stdout, stderr, exit := runCLI(t, []string{"--base-url", server.URL, "--token", bearerToken, "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", directory}, "")
			if stdout != "" || exit != ExitAuth || !strings.Contains(stderr, `"code":"`+codeName+`"`) ||
				strings.Contains(stderr, purposeToken) || strings.Contains(stderr, bearerToken) {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("partial files published: %#v", entries)
			}
		})
	}
}

func TestAnnotationGetRefreshesExpiredAndNotFoundLocatorsWithProgress(t *testing.T) {
	for _, mode := range []string{"expired", "not-found"} {
		t.Run(mode, func(t *testing.T) {
			png := []byte("\x89PNG\r\n\x1a\nrefreshed")
			var detailCalls atomic.Int32
			var oldMediaCalls atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/annotations/5":
					call := detailCalls.Add(1)
					token := "old-token"
					expires := time.Now().Add(5 * time.Minute)
					if call == 1 && mode == "expired" {
						expires = time.Now().Add(-time.Minute)
					}
					if call > 1 {
						token = "new-token"
					}
					writeAttachmentDetail(t, w, map[string]any{
						"id":          5,
						"attachments": []any{attachmentJSON(server.URL, 12, "image/png", png, token, expires)},
						"comments":    []any{},
					})
				case "/api/media/image_attachments/12":
					if r.URL.Query().Get("token") == "old-token" {
						oldMediaCalls.Add(1)
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte(`{"error":"not found","code":"not_found"}`))
						return
					}
					writeImage(w, "image/png", png)
				}
			}))
			defer server.Close()
			directory := filepath.Join(t.TempDir(), "attachments")

			_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", directory}, "")
			if code != ExitOK || stderr != "" || detailCalls.Load() != 2 {
				t.Fatalf("code=%d stderr=%s details=%d old_media=%d", code, stderr, detailCalls.Load(), oldMediaCalls.Load())
			}
			if mode == "expired" && oldMediaCalls.Load() != 0 {
				t.Fatal("expired locator was requested before refresh")
			}
			if mode == "not-found" && oldMediaCalls.Load() != 1 {
				t.Fatalf("old media calls = %d", oldMediaCalls.Load())
			}
			assertPrivateFile(t, filepath.Join(directory, "attachment-12.png"), png)
		})
	}
}

func TestAnnotationGetRejectsRefreshWithoutProgress(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nimage")
	var detailCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/5" {
			detailCalls.Add(1)
			writeAttachmentDetail(t, w, map[string]any{
				"id":          5,
				"attachments": []any{attachmentJSON(server.URL, 12, "image/png", png, "unchanged", time.Now().Add(5*time.Minute))},
				"comments":    []any{},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found","code":"not_found"}`))
	}))
	defer server.Close()

	_, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", filepath.Join(t.TempDir(), "attachments")}, "")
	if code != ExitGeneric || !strings.Contains(stderr, `"code":"attachment_refresh_failed"`) || detailCalls.Load() != 2 || strings.Contains(stderr, "unchanged") {
		t.Fatalf("code=%d stderr=%s detail_calls=%d", code, stderr, detailCalls.Load())
	}
}

func TestAnnotationGetComposesAttachmentsAndCropIntoOneDocument(t *testing.T) {
	encoded, crop := cropPNG(t)
	image := []byte("\x89PNG\r\n\x1a\nattachment")
	cropDirectory := t.TempDir()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/5" {
			writeAttachmentDetail(t, w, map[string]any{
				"id": 5, "cropped_image_base64": encoded, "mime_type": "image/png",
				"attachments": []any{attachmentJSON(server.URL, 12, "image/png", image, "token", time.Now().Add(5*time.Minute))},
				"comments":    []any{},
			})
			return
		}
		staged, err := filepath.Glob(filepath.Join(cropDirectory, ".screenote-crop-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(staged) != 0 {
			t.Fatalf("crop was staged before attachment download completed: %q", staged)
		}
		writeImage(w, "image/png", image)
	}))
	defer server.Close()
	directory := filepath.Join(t.TempDir(), "attachments")
	cropPath := filepath.Join(cropDirectory, "crop.png")

	stdout, stderr, code := runCLI(t, []string{"--base-url", server.URL, "--token", "key", "--project", "7", "annotation", "get", "--annotation", "5", "--attachments-dir", directory, "--crop-file", cropPath}, "")
	if code != ExitOK || stderr != "" || strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["crop_file"] != cropPath {
		t.Fatalf("payload = %#v", payload)
	}
	if _, exists := payload["cropped_image_base64"]; exists {
		t.Fatalf("crop base64 was not removed: %s", stdout)
	}
	assertPrivateFile(t, cropPath, crop)
	assertPrivateFile(t, filepath.Join(directory, "attachment-12.png"), image)
}

func attachmentJSON(baseURL string, id int, mediaType string, data []byte, token string, expires time.Time) map[string]any {
	return map[string]any{
		"id": id, "alt_text": nil, "media_type": mediaType, "width": 2, "height": 2, "size": len(data),
		"url":               fmt.Sprintf("%s/api/media/image_attachments/%d?token=%s", baseURL, id, token),
		"url_expires_at":    expires.UTC().Format(time.RFC3339),
		"future_attachment": map[string]any{"kept": id},
	}
}

func writeAttachmentDetail(t *testing.T, w http.ResponseWriter, payload map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatal(err)
	}
}

func assertAttachmentRequest(t *testing.T, request *http.Request, token string) {
	t.Helper()
	if request.Header.Get("Authorization") != "Bearer key" || request.Header.Get("Accept-Encoding") != "identity" || request.URL.Query().Get("token") != token {
		t.Fatalf("request headers=%#v url=%s", request.Header, request.URL.String())
	}
}

func writeImage(w http.ResponseWriter, mediaType string, data []byte) {
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	_, _ = w.Write(data)
}

func assertPrivateFile(t *testing.T, path string, expected []byte) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(contents, expected) {
		t.Fatalf("file %s bytes=%d err=%v", filepath.Base(path), len(contents), err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file %s mode=%#o err=%v", filepath.Base(path), info.Mode().Perm(), err)
	}
}
