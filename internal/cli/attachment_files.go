package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
)

const attachmentRefreshLead = 30 * time.Second

type rawObject map[string]json.RawMessage

type attachmentDetail struct {
	payload  rawObject
	root     []rawObject
	comments []attachmentComment
	items    []*attachmentExportItem
}

type attachmentComment struct {
	object      rawObject
	attachments []rawObject
}

type attachmentExportItem struct {
	original  screenote.AttachmentMetadata
	current   screenote.AttachmentMetadata
	object    rawObject
	filename  string
	localPath string
	temporary string
	published bool
}

type privateAttachmentDirectory struct {
	path string
	root *os.Root
}

func exportAnnotationAttachments(
	ctx context.Context,
	client *screenote.Client,
	annotationID, project, directoryPath, cropPath string,
	raw json.RawMessage,
) (map[string]json.RawMessage, error) {
	detail, err := parseAttachmentDetail(raw)
	if err != nil {
		return nil, err
	}

	directory, err := openPrivateAttachmentDirectory(directoryPath)
	if err != nil {
		return nil, err
	}
	defer directory.root.Close()

	for _, item := range detail.items {
		if err := client.ValidateAttachment(item.current); err != nil {
			return nil, attachmentValidationError(err)
		}
		item.filename = attachmentFilename(item.original)
		item.localPath = filepath.Join(directory.path, item.filename)
		if _, err := directory.root.Lstat(item.filename); err == nil {
			return nil, usageError("attachment_exists", "an attachment output file already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, usageError("invalid_attachments_directory", "attachment output directory cannot be inspected safely")
		}
	}

	var crop *preparedCrop
	if cropPath != "" {
		crop, err = prepareAnnotationCrop(detail.payload, cropPath)
		if err != nil {
			return nil, err
		}
		defer crop.cleanup()
		for _, item := range detail.items {
			if sameFilesystemPath(crop.destination, item.localPath) {
				return nil, usageError("output_collision", "--crop-file conflicts with an attachment output file")
			}
		}
		if err := crop.stage(); err != nil {
			return nil, err
		}
	}

	committed := false
	defer func() {
		if !committed {
			directory.rollback(detail.items)
		}
	}()

	refreshes := 0
	for index, item := range detail.items {
		for {
			if attachmentExpiresSoon(item.current, time.Now()) {
				if err := detail.refreshLocators(
					ctx, client, annotationID, project, index, item, &refreshes,
				); err != nil {
					return nil, err
				}
				continue
			}

			err := directory.stage(ctx, client, item)
			if isAttachmentNotFound(err) {
				if err := detail.refreshLocators(
					ctx, client, annotationID, project, index, item, &refreshes,
				); err != nil {
					return nil, err
				}
				continue
			}
			if err != nil {
				return nil, attachmentDownloadError(ctx, err)
			}
			break
		}
	}

	for _, item := range detail.items {
		if err := directory.root.Link(item.temporary, item.filename); err != nil {
			if errors.Is(err, os.ErrExist) {
				return nil, usageError("attachment_exists", "an attachment output file already exists")
			}
			return nil, genericError("attachment_write_failed", "attachment files could not be published")
		}
		item.published = true
		if err := directory.root.Remove(item.temporary); err != nil {
			return nil, genericError("attachment_write_failed", "attachment files could not be published")
		}
		item.temporary = ""
	}
	if err := directory.sync(); err != nil {
		return nil, genericError("attachment_write_failed", "attachment files could not be published")
	}
	if crop != nil {
		if err := crop.publish(); err != nil {
			return nil, err
		}
	}

	for _, item := range detail.items {
		delete(item.object, "url")
		delete(item.object, "url_expires_at")
		localPath, _ := json.Marshal(item.localPath)
		item.object["local_path"] = localPath
	}
	if crop != nil {
		crop.apply(detail.payload)
	}
	if err := detail.rebuild(); err != nil {
		return nil, genericError("invalid_response", "annotation response could not be transformed")
	}
	committed = true
	return detail.payload, nil
}

func parseAttachmentDetail(raw json.RawMessage) (*attachmentDetail, error) {
	var payload rawObject
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return nil, genericError("invalid_response", "annotation response is not valid JSON")
	}
	detail := &attachmentDetail{payload: payload}

	rootRaw, exists := payload["attachments"]
	if !exists {
		return nil, genericError("attachments_unsupported", "server does not expose annotation attachments")
	}
	root, metadata, err := parseAttachmentArray(rootRaw)
	if err != nil {
		return nil, err
	}
	detail.root = root
	for index := range root {
		detail.items = append(detail.items, &attachmentExportItem{
			original: metadata[index], current: metadata[index], object: root[index],
		})
	}

	commentsRaw, exists := payload["comments"]
	if !exists {
		return nil, genericError("invalid_response", "annotation response is missing comments")
	}
	commentObjects, err := parseObjectArray(commentsRaw)
	if err != nil {
		return nil, genericError("invalid_response", "annotation comments are invalid")
	}
	for _, commentObject := range commentObjects {
		attachmentsRaw, exists := commentObject["attachments"]
		if !exists {
			return nil, genericError("attachments_unsupported", "server does not expose comment attachments")
		}
		attachments, commentMetadata, err := parseAttachmentArray(attachmentsRaw)
		if err != nil {
			return nil, err
		}
		detail.comments = append(detail.comments, attachmentComment{object: commentObject, attachments: attachments})
		for index := range attachments {
			detail.items = append(detail.items, &attachmentExportItem{
				original: commentMetadata[index], current: commentMetadata[index], object: attachments[index],
			})
		}
	}

	seen := make(map[int]struct{}, len(detail.items))
	for _, item := range detail.items {
		if _, exists := seen[item.original.ID]; exists {
			return nil, genericError("invalid_attachment_metadata", "annotation attachment IDs are not unique")
		}
		seen[item.original.ID] = struct{}{}
	}
	return detail, nil
}

func parseObjectArray(raw json.RawMessage) ([]rawObject, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("null array")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	objects := make([]rawObject, len(entries))
	for index, entry := range entries {
		if err := json.Unmarshal(entry, &objects[index]); err != nil || objects[index] == nil {
			return nil, errors.New("invalid object")
		}
	}
	return objects, nil
}

func parseAttachmentArray(raw json.RawMessage) ([]rawObject, []screenote.AttachmentMetadata, error) {
	objects, err := parseObjectArray(raw)
	if err != nil {
		return nil, nil, genericError("invalid_attachment_metadata", "annotation attachments are invalid")
	}
	metadata := make([]screenote.AttachmentMetadata, len(objects))
	for index, object := range objects {
		metadata[index], err = parseAttachmentMetadata(object)
		if err != nil {
			return nil, nil, err
		}
	}
	return objects, metadata, nil
}

func parseAttachmentMetadata(object rawObject) (screenote.AttachmentMetadata, error) {
	type attachmentWire struct {
		ID           *int    `json:"id"`
		MediaType    *string `json:"media_type"`
		Width        *int    `json:"width"`
		Height       *int    `json:"height"`
		Size         *int64  `json:"size"`
		URL          *string `json:"url"`
		URLExpiresAt *string `json:"url_expires_at"`
	}
	raw, _ := json.Marshal(object)
	var wire attachmentWire
	if err := json.Unmarshal(raw, &wire); err != nil || wire.ID == nil || wire.MediaType == nil ||
		wire.Width == nil || wire.Height == nil || wire.Size == nil || wire.URL == nil || wire.URLExpiresAt == nil {
		return screenote.AttachmentMetadata{}, genericError("invalid_attachment_metadata", "annotation attachment metadata is incomplete")
	}
	altRaw, exists := object["alt_text"]
	if !exists {
		return screenote.AttachmentMetadata{}, genericError("invalid_attachment_metadata", "annotation attachment metadata is incomplete")
	}
	var altText *string
	if err := json.Unmarshal(altRaw, &altText); err != nil {
		return screenote.AttachmentMetadata{}, genericError("invalid_attachment_metadata", "annotation attachment metadata is invalid")
	}
	metadata := screenote.AttachmentMetadata{
		ID: *wire.ID, MediaType: *wire.MediaType, Width: *wire.Width, Height: *wire.Height,
		Size: *wire.Size, URL: *wire.URL, URLExpiresAt: *wire.URLExpiresAt, AltText: altText,
	}
	if metadata.ID <= 0 || metadata.Width <= 0 || metadata.Height <= 0 ||
		metadata.Width > 32_768 || metadata.Height > 32_768 || int64(metadata.Width)*int64(metadata.Height) > 50_000_000 ||
		metadata.Size <= 0 ||
		metadata.Size > screenote.MaxImageAttachmentBytes || attachmentExtension(metadata.MediaType) == "" {
		return screenote.AttachmentMetadata{}, genericError("invalid_attachment_metadata", "annotation attachment metadata is invalid")
	}
	if _, err := time.Parse(time.RFC3339, metadata.URLExpiresAt); err != nil {
		return screenote.AttachmentMetadata{}, genericError("invalid_attachment_metadata", "annotation attachment expiry is invalid")
	}
	return metadata, nil
}

func (detail *attachmentDetail) rebuild() error {
	root, err := json.Marshal(detail.root)
	if err != nil {
		return err
	}
	detail.payload["attachments"] = root
	comments := make([]rawObject, len(detail.comments))
	for index, comment := range detail.comments {
		attachments, err := json.Marshal(comment.attachments)
		if err != nil {
			return err
		}
		comment.object["attachments"] = attachments
		comments[index] = comment.object
	}
	commentsRaw, err := json.Marshal(comments)
	if err != nil {
		return err
	}
	detail.payload["comments"] = commentsRaw
	return nil
}

func openPrivateAttachmentDirectory(value string) (*privateAttachmentDirectory, error) {
	if strings.TrimSpace(value) == "" {
		return nil, usageError("invalid_attachments_directory", "--attachments-dir must name a local directory")
	}
	for _, component := range strings.Split(filepath.ToSlash(value), "/") {
		if component == ".." {
			return nil, usageError("invalid_attachments_directory", "--attachments-dir must not contain parent traversal")
		}
	}
	absolute, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return nil, usageError("invalid_attachments_directory", "attachment output directory is invalid")
	}

	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		if parentErr != nil {
			return nil, usageError("invalid_attachments_directory", "attachment output parent directory cannot be inspected safely")
		}
		absolute = filepath.Join(parent, filepath.Base(absolute))
		if err := os.Mkdir(absolute, 0o700); err != nil {
			return nil, genericError("attachment_write_failed", "attachment output directory could not be created")
		}
		info, err = os.Lstat(absolute)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return nil, usageError("invalid_attachments_directory", "attachment output must be a non-writable real directory")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, usageError("invalid_attachments_directory", "attachment output directory cannot be inspected safely")
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, usageError("invalid_attachments_directory", "attachment output directory cannot be opened safely")
	}
	rootInfo, rootErr := root.Stat(".")
	currentInfo, currentErr := os.Lstat(absolute)
	if rootErr != nil || currentErr != nil || currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(currentInfo, rootInfo) {
		_ = root.Close()
		return nil, usageError("invalid_attachments_directory", "attachment output directory changed during validation")
	}
	return &privateAttachmentDirectory{path: canonical, root: root}, nil
}

func (directory *privateAttachmentDirectory) stage(ctx context.Context, client *screenote.Client, item *attachmentExportItem) error {
	file, temporary, err := directory.createTemporary()
	if err != nil {
		return genericError("attachment_write_failed", "attachment file could not be staged")
	}
	item.temporary = temporary
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = directory.root.Remove(temporary)
			item.temporary = ""
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return genericError("attachment_write_failed", "attachment file could not be staged")
	}
	if err := client.DownloadAttachment(ctx, item.current, file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return genericError("attachment_write_failed", "attachment file could not be staged")
	}
	if err := file.Close(); err != nil {
		return genericError("attachment_write_failed", "attachment file could not be staged")
	}
	keep = true
	return nil
}

func (directory *privateAttachmentDirectory) createTemporary() (*os.File, string, error) {
	for range 16 {
		var entropy [12]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			return nil, "", err
		}
		name := ".screenote-attachment-" + base64.RawURLEncoding.EncodeToString(entropy[:])
		file, err := directory.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("temporary attachment names are exhausted")
}

func (directory *privateAttachmentDirectory) rollback(items []*attachmentExportItem) {
	for _, item := range items {
		if item.temporary != "" {
			_ = directory.root.Remove(item.temporary)
			item.temporary = ""
		}
		if item.published {
			_ = directory.root.Remove(item.filename)
			item.published = false
		}
	}
	_ = directory.sync()
}

func (directory *privateAttachmentDirectory) sync() error {
	file, err := directory.root.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func (detail *attachmentDetail) refreshLocators(
	ctx context.Context,
	client *screenote.Client,
	annotationID, project string,
	pendingStart int,
	trigger *attachmentExportItem,
	refreshes *int,
) error {
	if *refreshes >= len(detail.items)+1 {
		return genericError("attachment_refresh_failed", "attachment media could not be refreshed safely")
	}
	*refreshes++
	raw, err := client.Annotation(ctx, annotationID, project)
	if err != nil {
		return attachmentRefreshError(ctx, err)
	}
	refreshed, err := parseAttachmentDetail(raw)
	if err != nil {
		return genericError("attachment_refresh_failed", "attachment metadata refresh was invalid")
	}
	byID := make(map[int]screenote.AttachmentMetadata, len(refreshed.items))
	for _, candidate := range refreshed.items {
		byID[candidate.original.ID] = candidate.original
	}

	candidates := make([]screenote.AttachmentMetadata, len(detail.items)-pendingStart)
	for index, item := range detail.items[pendingStart:] {
		candidate, exists := byID[item.original.ID]
		if !exists || !sameStableAttachment(item.original, candidate) {
			return genericError("attachment_refresh_failed", "attachment metadata changed during refresh")
		}
		if err := client.ValidateAttachment(candidate); err != nil {
			return genericError("attachment_refresh_failed", "attachment metadata refresh was invalid")
		}
		candidates[index] = candidate
	}
	triggerCandidate := byID[trigger.original.ID]
	if triggerCandidate.URL == trigger.current.URL && triggerCandidate.URLExpiresAt == trigger.current.URLExpiresAt {
		return genericError("attachment_refresh_failed", "attachment media refresh made no progress")
	}
	for index, candidate := range candidates {
		detail.items[pendingStart+index].current = candidate
	}
	return nil
}

func sameStableAttachment(left, right screenote.AttachmentMetadata) bool {
	return left.ID == right.ID && left.MediaType == right.MediaType && left.Width == right.Width &&
		left.Height == right.Height && left.Size == right.Size && equalOptionalString(left.AltText, right.AltText)
}

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func attachmentExpiresSoon(metadata screenote.AttachmentMetadata, now time.Time) bool {
	expiresAt, err := time.Parse(time.RFC3339, metadata.URLExpiresAt)
	return err != nil || !expiresAt.After(now.Add(attachmentRefreshLead))
}

func isAttachmentNotFound(err error) bool {
	var apiErr *screenote.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func attachmentValidationError(err error) error {
	var downloadErr *screenote.AttachmentDownloadError
	if errors.As(err, &downloadErr) && downloadErr.Code == "invalid_attachment_url" {
		return genericError("invalid_attachment_url", "attachment media URL is not trusted")
	}
	return genericError("invalid_attachment_metadata", "annotation attachment metadata is invalid")
}

func attachmentDownloadError(ctx context.Context, err error) error {
	if isCanceled(ctx, err) {
		return genericError("request_canceled", "attachment download was canceled")
	}
	var cliErr *cliError
	if errors.As(err, &cliErr) {
		return err
	}
	var apiErr *screenote.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return authError(apiErr.Code, "attachment download was not authorized")
		case http.StatusTooManyRequests:
			return &screenote.Error{StatusCode: apiErr.StatusCode, Code: apiErr.Code, Message: "attachment download was rate limited"}
		}
	}
	return genericError("attachment_download_failed", "attachment could not be downloaded safely")
}

func attachmentRefreshError(ctx context.Context, err error) error {
	if isCanceled(ctx, err) {
		return genericError("request_canceled", "attachment refresh was canceled")
	}
	var apiErr *screenote.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return authError(apiErr.Code, "attachment refresh was not authorized")
		case http.StatusTooManyRequests:
			return &screenote.Error{StatusCode: apiErr.StatusCode, Code: apiErr.Code, Message: "attachment refresh was rate limited"}
		}
	}
	return genericError("attachment_refresh_failed", "attachment media could not be refreshed safely")
}

func attachmentExtension(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	default:
		return ""
	}
}

func attachmentFilename(metadata screenote.AttachmentMetadata) string {
	return "attachment-" + strconv.Itoa(metadata.ID) + "." + attachmentExtension(metadata.MediaType)
}

func sameFilesystemPath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}
