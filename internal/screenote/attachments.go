package screenote

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const maxAttachmentErrorBytes int64 = 64 << 10

// AttachmentDownloadError reports a safe, token-free validation failure.
type AttachmentDownloadError struct {
	Code string
}

func (e *AttachmentDownloadError) Error() string {
	switch e.Code {
	case "invalid_attachment_url":
		return "attachment media URL is invalid"
	case "attachment_redirect":
		return "attachment media redirected"
	default:
		return "attachment media response is invalid"
	}
}

// ValidateAttachment proves the expiring locator addresses the expected
// attachment on this client's configured origin. It performs no network I/O.
func (c *Client) ValidateAttachment(metadata AttachmentMetadata) error {
	if metadata.ID <= 0 || metadata.Size <= 0 || metadata.Size > MaxImageAttachmentBytes ||
		!supportedAttachmentMediaType(metadata.MediaType) {
		return &AttachmentDownloadError{Code: "invalid_attachment_data"}
	}

	mediaURL, err := url.Parse(metadata.URL)
	if err != nil || mediaURL.Scheme == "" || mediaURL.Host == "" || mediaURL.Opaque != "" ||
		mediaURL.User != nil || mediaURL.Fragment != "" || mediaURL.ForceQuery || mediaURL.RawPath != "" ||
		mediaURL.Scheme != c.baseURL.Scheme || mediaURL.Host != c.baseURL.Host {
		return &AttachmentDownloadError{Code: "invalid_attachment_url"}
	}
	expectedPath := strings.TrimRight(c.baseURL.Path, "/") + "/api/media/image_attachments/" + strconv.Itoa(metadata.ID)
	if mediaURL.Path != expectedPath {
		return &AttachmentDownloadError{Code: "invalid_attachment_url"}
	}
	query, err := url.ParseQuery(mediaURL.RawQuery)
	if err != nil || len(query) != 1 {
		return &AttachmentDownloadError{Code: "invalid_attachment_url"}
	}
	tokens, exists := query["token"]
	if !exists || len(tokens) != 1 || tokens[0] == "" {
		return &AttachmentDownloadError{Code: "invalid_attachment_url"}
	}
	return nil
}

// DownloadAttachment streams one already-validated private attachment without
// following redirects. The bearer header is attached only after origin and
// path validation, and the bytes must exactly match the serialized identity.
func (c *Client) DownloadAttachment(ctx context.Context, metadata AttachmentMetadata, destination io.Writer) error {
	if err := c.ValidateAttachment(metadata); err != nil {
		return err
	}
	if c.bearerToken == "" {
		return &AttachmentDownloadError{Code: "missing_attachment_credential"}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, metadata.URL, nil)
	if err != nil {
		return &AttachmentDownloadError{Code: "invalid_attachment_url"}
	}
	request.Header.Set("Authorization", "Bearer "+c.bearerToken)
	request.Header.Set("Accept", metadata.MediaType)
	request.Header.Set("Accept-Encoding", "identity")

	downloadClient := *c.httpClient
	downloadClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := downloadClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 && response.StatusCode <= 399 {
		return &AttachmentDownloadError{Code: "attachment_redirect"}
	}
	if response.StatusCode != http.StatusOK {
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, maxAttachmentErrorBytes+1))
		if readErr != nil {
			return &AttachmentDownloadError{Code: "invalid_attachment_data"}
		}
		return parseError(response.StatusCode, raw)
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return &AttachmentDownloadError{Code: "invalid_attachment_data"}
	}
	responseType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || responseType != metadata.MediaType {
		return &AttachmentDownloadError{Code: "invalid_attachment_data"}
	}
	if response.ContentLength >= 0 && response.ContentLength != metadata.Size {
		return &AttachmentDownloadError{Code: "invalid_attachment_data"}
	}

	prefixSize := min(metadata.Size, 12)
	prefix := make([]byte, prefixSize)
	if _, err := io.ReadFull(response.Body, prefix); err != nil || !matchesAttachmentSignature(metadata.MediaType, prefix) {
		return &AttachmentDownloadError{Code: "invalid_attachment_data"}
	}
	if _, err := destination.Write(prefix); err != nil {
		return fmt.Errorf("write attachment: %w", err)
	}
	remaining := metadata.Size - prefixSize
	written, err := io.Copy(destination, io.LimitReader(response.Body, remaining+1))
	if err != nil || written != remaining {
		return &AttachmentDownloadError{Code: "invalid_attachment_data"}
	}
	return nil
}

func supportedAttachmentMediaType(mediaType string) bool {
	return mediaType == "image/png" || mediaType == "image/jpeg" || mediaType == "image/webp"
}

func matchesAttachmentSignature(mediaType string, prefix []byte) bool {
	switch mediaType {
	case "image/png":
		return len(prefix) >= 8 && string(prefix[:8]) == "\x89PNG\r\n\x1a\n"
	case "image/jpeg":
		return len(prefix) >= 3 && prefix[0] == 0xff && prefix[1] == 0xd8 && prefix[2] == 0xff
	case "image/webp":
		return len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WEBP"
	default:
		return false
	}
}
