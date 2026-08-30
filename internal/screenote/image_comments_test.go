package screenote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientAddImageCommentUsesDedicatedMultipartContract(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\nimage-bytes")
	digest := sha256.Sum256(image)
	key := "0123456789abcdef0123456789abcdef"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/annotations/5/image_comments" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != key {
			t.Fatalf("Idempotency-Key = %q", got)
		}
		if got := r.Header.Get("Screenote-Image-SHA256"); got != hex.EncodeToString(digest[:]) {
			t.Fatalf("Screenote-Image-SHA256 = %q", got)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		defer r.MultipartForm.RemoveAll()
		if got := r.FormValue("project_id"); got != "7" {
			t.Fatalf("project_id = %q", got)
		}
		if got := r.FormValue("body"); got != "change this element" {
			t.Fatalf("body = %q", got)
		}
		files := r.MultipartForm.File["images[]"]
		if len(files) != 1 {
			t.Fatalf("images[] count = %d", len(files))
		}
		if files[0].Filename != "attachment.png" || files[0].Header.Get("Content-Type") != "image/png" {
			t.Fatalf("file = %#v", files[0])
		}
		file, err := files[0].Open()
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		got, err := io.ReadAll(file)
		if err != nil || string(got) != string(image) {
			t.Fatalf("image = %q err=%v", got, err)
		}
		w.Header().Set("Screenote-API-Capability", ImageCommentsCapability)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"operation":"created","comment":{"id":11},"attachment":{"id":12}}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.AddImageComment(context.Background(), "5", "7", "change this element", key, ImageCommentUpload{
		Filename: "attachment.png", ContentType: "image/png", SHA256: hex.EncodeToString(digest[:]), Body: strings.NewReader(string(image)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"operation":"created"`) {
		t.Fatalf("raw = %s", raw)
	}
}

func TestClientErrorRecordsImageCommentCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Screenote-API-Capability", ImageCommentsCapability)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Annotation not found","code":"annotation_not_found"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AddImageComment(context.Background(), "5", "7", "body", "0123456789abcdef", ImageCommentUpload{
		Filename: "attachment.png", ContentType: "image/png", SHA256: strings.Repeat("0", 64), Body: strings.NewReader("png"),
	})
	apiErr, ok := err.(*Error)
	if !ok || apiErr.Capability != ImageCommentsCapability || apiErr.Code != "annotation_not_found" {
		t.Fatalf("error = %#v", err)
	}
}
