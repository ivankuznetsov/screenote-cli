package screenote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if oauthClient := httpClientOrDefault(nil); oauthClient.Timeout != defaultHTTPTimeout {
		t.Fatalf("OAuth timeout=%s want %s", oauthClient.Timeout, defaultHTTPTimeout)
	}
}
