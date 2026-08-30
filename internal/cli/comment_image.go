package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
)

const maxCommentImageBytes int64 = 20 << 20

type singleImageFlag struct {
	value string
	count int
}

func (f *singleImageFlag) String() string { return f.value }
func (f *singleImageFlag) Type() string   { return "path" }
func (f *singleImageFlag) Set(value string) error {
	f.count++
	if f.count == 1 {
		f.value = value
	}
	return nil
}

type commentImage struct {
	path        string
	filename    string
	contentType string
	sha256      string
}

func (image *commentImage) cleanup() {
	if image != nil && image.path != "" {
		_ = os.Remove(image.path)
	}
}

func (a *app) addImageComment(ctx context.Context, annotationID, body, source string) error {
	image, err := spoolCommentImage(a.stdin, source)
	if err != nil {
		return err
	}
	defer image.cleanup()

	key, err := newImageCommentKey()
	if err != nil {
		return genericError("image_prepare_failed", "image comment could not be prepared")
	}
	client, project, err := a.clientForProject(ctx)
	if err != nil {
		return err
	}

	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return genericError("request_canceled", "image comment request was canceled")
		}
		file, err := os.Open(image.path)
		if err != nil {
			return genericError("image_read_failed", "image input could not be read")
		}
		raw, requestErr := client.AddImageComment(ctx, annotationID, project, body, key, screenote.ImageCommentUpload{
			Filename: image.filename, ContentType: image.contentType, SHA256: image.sha256, Body: file,
		})
		closeErr := file.Close()
		if requestErr == nil && closeErr != nil {
			requestErr = closeErr
		}
		if requestErr == nil {
			return writeRawJSON(a.stdout, raw)
		}
		if isCanceled(ctx, requestErr) {
			return genericError("request_canceled", "image comment request was canceled")
		}
		if imageCommentsUnsupported(requestErr) {
			return genericError("image_comments_unsupported", "server does not support image comments")
		}
		if !ambiguousImageCommentError(requestErr) {
			return requestErr
		}
	}

	return genericError("comment_result_unknown", "image comment result is unknown; retrying with a new command may create another comment")
}

func spoolCommentImage(stdin io.Reader, source string) (_ *commentImage, resultErr error) {
	var input io.Reader = stdin
	var sourceFile *os.File
	if source != "-" {
		if source == "" {
			return nil, usageError("invalid_image", "--image must name a local file or - for stdin")
		}
		var err error
		sourceFile, err = os.Open(source)
		if err != nil {
			return nil, usageError("image_read_failed", "image input could not be read")
		}
		defer sourceFile.Close()
		input = sourceFile
	}

	temporary, err := os.CreateTemp("", "screenote-comment-image-*")
	if err != nil {
		return nil, genericError("image_prepare_failed", "image comment could not be prepared")
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if resultErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return nil, genericError("image_prepare_failed", "image comment could not be prepared")
	}

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(temporary, hasher), io.LimitReader(input, maxCommentImageBytes+1))
	if err != nil {
		return nil, usageError("image_read_failed", "image input could not be read")
	}
	if size == 0 {
		return nil, usageError("empty_image", "image input is empty")
	}
	if size > maxCommentImageBytes {
		return nil, usageError("image_too_large", "image exceeds the 20 MiB limit")
	}
	if err := temporary.Sync(); err != nil {
		return nil, genericError("image_prepare_failed", "image comment could not be prepared")
	}

	var prefix [12]byte
	n, err := temporary.ReadAt(prefix[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, genericError("image_prepare_failed", "image comment could not be prepared")
	}
	contentType, extension := sniffCommentImage(prefix[:n])
	if contentType == "" {
		return nil, usageError("unsupported_image_type", "image must be PNG, JPEG, or WebP")
	}
	if err := temporary.Close(); err != nil {
		return nil, genericError("image_prepare_failed", "image comment could not be prepared")
	}

	return &commentImage{
		path:        temporaryPath,
		filename:    "attachment." + extension,
		contentType: contentType,
		sha256:      hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

func sniffCommentImage(prefix []byte) (contentType, extension string) {
	switch {
	case len(prefix) >= 8 && string(prefix[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", "png"
	case len(prefix) >= 3 && prefix[0] == 0xff && prefix[1] == 0xd8 && prefix[2] == 0xff:
		return "image/jpeg", "jpg"
	case len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WEBP":
		return "image/webp", "webp"
	default:
		return "", ""
	}
}

func newImageCommentKey() (string, error) {
	var entropy [24]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(entropy[:]), nil
}

func imageCommentsUnsupported(err error) bool {
	var apiErr *screenote.Error
	return errors.As(err, &apiErr) &&
		(apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusMethodNotAllowed) &&
		apiErr.Capability != screenote.ImageCommentsCapability
}

func ambiguousImageCommentError(err error) bool {
	var apiErr *screenote.Error
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.StatusCode == http.StatusBadGateway ||
		apiErr.StatusCode == http.StatusServiceUnavailable ||
		apiErr.StatusCode == http.StatusGatewayTimeout
}

func isCanceled(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
