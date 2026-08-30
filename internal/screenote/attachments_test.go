package screenote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDownloadAttachmentValidatesOriginAndStreamsExactPrivateBytes(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\nverified-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/media/image_attachments/12" || r.URL.Query().Get("token") != "purpose-token" {
			t.Fatalf("url = %s", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer bearer-secret" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Fatalf("Accept-Encoding = %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "22")
		_, _ = w.Write(image)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "bearer-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata := AttachmentMetadata{ID: 12, MediaType: "image/png", Size: int64(len(image)), URL: server.URL + "/api/media/image_attachments/12?token=purpose-token"}
	if err := client.ValidateAttachment(metadata); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := client.DownloadAttachment(context.Background(), metadata, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), image) {
		t.Fatalf("output = %q", output.Bytes())
	}
}

func TestValidateAttachmentRejectsHostileLocatorsBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client, err := NewClient(server.URL, "bearer-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}

	tests := []string{
		"https://other.example/api/media/image_attachments/12?token=x",
		strings.Replace(server.URL, "http://", "http://user@", 1) + "/api/media/image_attachments/12?token=x",
		server.URL + "/api/media/image_attachments/13?token=x",
		server.URL + "/api/media/image_attachments/12?token=x&token=y",
		server.URL + "/api/media/image_attachments/12",
		server.URL + "/api/media/image_attachments/12?token=x#secret",
		server.URL + "/api/media/image_attachments/12?token=x&next=https://other.example",
	}
	for _, locator := range tests {
		metadata := AttachmentMetadata{ID: 12, MediaType: "image/png", Size: 8, URL: locator}
		if err := client.ValidateAttachment(metadata); err == nil {
			t.Fatalf("accepted locator %q", locator)
		}
		if err := client.DownloadAttachment(context.Background(), metadata, io.Discard); err == nil {
			t.Fatalf("download accepted locator %q", locator)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("requests = %d", calls.Load())
	}
}

func TestDownloadAttachmentNeverForwardsBearerAcrossRedirect(t *testing.T) {
	var leaked atomic.Bool
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/stolen", http.StatusFound)
	}))
	defer source.Close()
	client, err := NewClient(source.URL, "bearer-secret", source.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata := AttachmentMetadata{ID: 12, MediaType: "image/png", Size: 8, URL: source.URL + "/api/media/image_attachments/12?token=purpose-token"}
	err = client.DownloadAttachment(context.Background(), metadata, io.Discard)
	var downloadErr *AttachmentDownloadError
	if !errors.As(err, &downloadErr) || downloadErr.Code != "attachment_redirect" {
		t.Fatalf("error = %#v", err)
	}
	if leaked.Load() {
		t.Fatal("bearer credential crossed the redirect")
	}
}

func TestDownloadAttachmentRejectsInvalidBodies(t *testing.T) {
	tests := []struct {
		name        string
		mediaType   string
		body        []byte
		declaredLen int
	}{
		{name: "truncated", mediaType: "image/png", body: []byte("\x89PNG\r\n\x1a\n"), declaredLen: 20},
		{name: "oversized", mediaType: "image/png", body: []byte("\x89PNG\r\n\x1a\nextra"), declaredLen: 8},
		{name: "wrong signature", mediaType: "image/png", body: []byte("not-png!"), declaredLen: 8},
		{name: "wrong response type", mediaType: "image/jpeg", body: []byte("\xff\xd8\xffbytes"), declaredLen: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				responseType := test.mediaType
				if test.name == "wrong response type" {
					responseType = "image/png"
				}
				w.Header().Set("Content-Type", responseType)
				_, _ = w.Write(test.body)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "token", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			metadata := AttachmentMetadata{
				ID: 12, MediaType: test.mediaType, Size: int64(test.declaredLen),
				URL: server.URL + "/api/media/image_attachments/12?token=purpose-token",
			}
			var output bytes.Buffer
			err = client.DownloadAttachment(context.Background(), metadata, &output)
			var downloadErr *AttachmentDownloadError
			if !errors.As(err, &downloadErr) || downloadErr.Code != "invalid_attachment_data" {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}
