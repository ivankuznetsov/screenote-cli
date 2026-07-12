package snapshot

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	Version         = 1
	MaxImages       = 100
	MaxFileSize     = 20 << 20
	maxFutureSkew   = 5 * time.Minute
	canonicalLayout = "2006-01-02T15:04:05.000000Z"
)

var gitCommitPattern = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

type Manifest struct {
	Version   int          `json:"version"`
	GitCommit string       `json:"git_commit"`
	TakenAt   string       `json:"taken_at"`
	Images    []ImageEntry `json:"images"`
}

type ImageEntry struct {
	Page     string `json:"page"`
	File     string `json:"file"`
	Viewport string `json:"viewport"`
	Title    string `json:"title,omitempty"`
}

type PreparedManifest struct {
	Version        int
	GitCommit      string
	TakenAt        string
	ManifestDigest string
	Images         []PreparedImage
}

type PreparedImage struct {
	Index         int
	Page          string
	Title         string
	Viewport      string
	MIMEType      string
	ContentSHA256 string
	FileRefSHA256 string
	GroupDigest   string
	Size          int64
	rootPath      string
	fileRef       string
}

type ValidationError struct {
	Code    string
	Message string
	Index   *int
}

func (e *ValidationError) Error() string { return e.Message }

func Load(path string) (*PreparedManifest, error) {
	return load(path, time.Now())
}

func load(path string, now time.Time) (*PreparedManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, validationError("manifest_unreadable", "manifest cannot be read", nil)
	}
	defer file.Close()

	var manifest Manifest
	decoder := json.NewDecoder(bufio.NewReader(file))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, validationError("invalid_manifest", "manifest is not valid JSON", nil)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, validationError("invalid_manifest", "manifest must contain one JSON object", nil)
	}

	return prepareManifest(filepath.Dir(path), manifest, now)
}

func prepareManifest(manifestDir string, manifest Manifest, now time.Time) (*PreparedManifest, error) {
	if manifest.Version != Version {
		return nil, validationError("unsupported_manifest_version", "manifest version must be 1", nil)
	}

	gitCommit := strings.ToLower(strings.TrimSpace(manifest.GitCommit))
	if !gitCommitPattern.MatchString(gitCommit) {
		return nil, validationError("invalid_git_commit", "git_commit must be 7-40 hexadecimal characters", nil)
	}

	takenAt, err := time.Parse(time.RFC3339Nano, manifest.TakenAt)
	if err != nil || !hasExplicitOffset(manifest.TakenAt) {
		return nil, validationError("invalid_taken_at", "taken_at must be an ISO 8601 timestamp with an explicit UTC offset", nil)
	}
	if takenAt.After(now.Add(maxFutureSkew)) {
		return nil, validationError("invalid_taken_at", "taken_at cannot be in the future", nil)
	}
	normalizedTakenAt := takenAt.UTC().Format(canonicalLayout)

	if len(manifest.Images) < 1 || len(manifest.Images) > MaxImages {
		return nil, validationError("invalid_image_count", "images must contain 1-100 entries", nil)
	}

	prepared := &PreparedManifest{
		Version:   Version,
		GitCommit: gitCommit,
		TakenAt:   normalizedTakenAt,
		Images:    make([]PreparedImage, 0, len(manifest.Images)),
	}
	rootPath, err := filepath.Abs(manifestDir)
	if err != nil {
		return nil, validationError("manifest_unreadable", "manifest directory cannot be read", nil)
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return nil, validationError("manifest_unreadable", "manifest directory cannot be read", nil)
	}
	seen := make(map[[3]string]struct{}, len(manifest.Images))
	for index, entry := range manifest.Images {
		image, err := prepareImage(rootPath, index, entry)
		if err != nil {
			return nil, err
		}
		key := [3]string{image.Page, image.Title, image.Viewport}
		if _, exists := seen[key]; exists {
			return nil, validationError("duplicate_viewport", "viewport must be unique within each page and title group", &index)
		}
		seen[key] = struct{}{}
		prepared.Images = append(prepared.Images, image)
	}

	assignGroupDigests(prepared.Images)
	components := []string{strconv.Itoa(Version), gitCommit, normalizedTakenAt, strconv.Itoa(len(prepared.Images))}
	for _, image := range prepared.Images {
		components = append(components,
			image.Page, image.Title, image.Viewport, image.MIMEType, image.ContentSHA256, image.FileRefSHA256,
		)
	}
	prepared.ManifestDigest = digest("screenote-manifest-v1", components)
	return prepared, nil
}

func prepareImage(rootPath string, index int, entry ImageEntry) (PreparedImage, error) {
	page, err := normalizeLabel(entry.Page, "page", index)
	if err != nil {
		return PreparedImage{}, err
	}
	title := page
	if entry.Title != "" {
		title, err = normalizeLabel(entry.Title, "title", index)
		if err != nil {
			return PreparedImage{}, err
		}
	}

	viewport := strings.ToLower(strings.TrimSpace(entry.Viewport))
	if viewport != "desktop" && viewport != "tablet" && viewport != "mobile" {
		return PreparedImage{}, validationError("invalid_viewport", "viewport must be desktop, tablet, or mobile", &index)
	}

	cleanRef, err := resolveLocalFile(rootPath, entry.File, index)
	if err != nil {
		return PreparedImage{}, err
	}
	file, err := openImage(rootPath, cleanRef, index)
	if err != nil {
		return PreparedImage{}, err
	}
	contentSHA, mimeType, size, inspectErr := inspectImage(file, index)
	closeErr := file.Close()
	if inspectErr != nil {
		return PreparedImage{}, inspectErr
	}
	if closeErr != nil {
		return PreparedImage{}, validationError("image_unreadable", "image file cannot be read", &index)
	}

	return PreparedImage{
		Index:         index,
		Page:          page,
		Title:         title,
		Viewport:      viewport,
		MIMEType:      mimeType,
		ContentSHA256: contentSHA,
		FileRefSHA256: sha256Hex([]byte(filepath.ToSlash(cleanRef))),
		Size:          size,
		rootPath:      rootPath,
		fileRef:       cleanRef,
	}, nil
}

func normalizeLabel(value, field string, index int) (string, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return "", validationError("invalid_"+field, field+" cannot be blank", &index)
	}
	if utf8.RuneCountInString(normalized) > 255 {
		return "", validationError("invalid_"+field, field+" is too long", &index)
	}
	return normalized, nil
}

func resolveLocalFile(rootPath, reference string, index int) (string, error) {
	if strings.TrimSpace(reference) == "" {
		return "", validationError("invalid_file", "file cannot be blank", &index)
	}
	if filepath.IsAbs(reference) {
		return "", validationError("invalid_file", "file must be relative to the manifest", &index)
	}
	clean := filepath.Clean(reference)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", validationError("invalid_file", "file must stay within the manifest directory", &index)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(rootPath, clean))
	if err != nil {
		return "", validationError("image_unreadable", "image file cannot be read", &index)
	}
	relative, err := filepath.Rel(rootPath, resolved)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", validationError("invalid_file", "file must stay within the manifest directory", &index)
	}
	return clean, nil
}

func openImage(rootPath, reference string, index int) (*os.File, error) {
	file, err := os.OpenInRoot(rootPath, reference)
	if err != nil {
		return nil, validationError("image_unreadable", "image file cannot be read", &index)
	}
	return file, nil
}

func inspectImage(file *os.File, index int) (string, string, int64, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", "", 0, validationError("image_unreadable", "image file must be a regular readable file", &index)
	}
	if info.Size() == 0 {
		return "", "", 0, validationError("empty_image", "image file is empty", &index)
	}
	if info.Size() > MaxFileSize {
		return "", "", 0, validationError("image_too_large", "image file exceeds the 20MB limit", &index)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", 0, validationError("image_unreadable", "image file cannot be read", &index)
	}

	_, format, err := image.DecodeConfig(io.LimitReader(file, MaxFileSize+1))
	if err != nil || (format != "png" && format != "jpeg") {
		return "", "", 0, validationError("invalid_image", "image file must contain valid PNG or JPEG bytes", &index)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", 0, validationError("image_unreadable", "image file cannot be read", &index)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, MaxFileSize+1))
	if err != nil {
		return "", "", 0, validationError("image_unreadable", "image file cannot be read", &index)
	}
	if written > MaxFileSize {
		return "", "", 0, validationError("image_too_large", "image file exceeds the 20MB limit", &index)
	}
	mimeType := "image/png"
	if format == "jpeg" {
		mimeType = "image/jpeg"
	}
	return hex.EncodeToString(hash.Sum(nil)), mimeType, written, nil
}

func assignGroupDigests(images []PreparedImage) {
	type groupKey struct{ page, title string }
	type group struct {
		key     groupKey
		indexes []int
	}
	groups := make([]group, 0)
	positions := make(map[groupKey]int)
	for index, image := range images {
		key := groupKey{page: image.Page, title: image.Title}
		position, exists := positions[key]
		if !exists {
			position = len(groups)
			positions[key] = position
			groups = append(groups, group{key: key})
		}
		groups[position].indexes = append(groups[position].indexes, index)
	}
	for _, group := range groups {
		components := []string{group.key.page, group.key.title, strconv.Itoa(len(group.indexes))}
		for _, index := range group.indexes {
			image := images[index]
			components = append(components, image.Viewport, image.MIMEType, image.ContentSHA256, image.FileRefSHA256)
		}
		groupDigest := digest("screenote-screenshot-v1", components)
		for _, index := range group.indexes {
			images[index].GroupDigest = groupDigest
		}
	}
}

func digest(namespace string, components []string) string {
	hash := sha256.New()
	_, _ = io.WriteString(hash, namespace)
	_, _ = hash.Write([]byte{0})
	for _, component := range components {
		_, _ = io.WriteString(hash, strconv.Itoa(len([]byte(component))))
		_, _ = io.WriteString(hash, ":")
		_, _ = io.WriteString(hash, component)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func hasExplicitOffset(value string) bool {
	if strings.HasSuffix(value, "Z") || strings.HasSuffix(value, "z") {
		return true
	}
	if len(value) < 6 {
		return false
	}
	suffix := value[len(value)-6:]
	return (suffix[0] == '+' || suffix[0] == '-') && suffix[3] == ':'
}

func validationError(code, message string, index *int) error {
	return &ValidationError{Code: code, Message: message, Index: index}
}

type verifiedUpload struct {
	file     *os.File
	path     string
	once     sync.Once
	closeErr error
}

func (upload *verifiedUpload) Read(buffer []byte) (int, error) {
	return upload.file.Read(buffer)
}

func (upload *verifiedUpload) Close() error {
	upload.once.Do(func() {
		closeErr := upload.file.Close()
		removeErr := os.Remove(upload.path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		upload.closeErr = errors.Join(closeErr, removeErr)
	})
	return upload.closeErr
}

func (image PreparedImage) OpenVerified() (io.ReadCloser, error) {
	source, err := openImage(image.rootPath, image.fileRef, image.Index)
	if err != nil {
		return nil, err
	}
	defer source.Close()

	temporary, err := os.CreateTemp("", "screenote-snapshot-*")
	if err != nil {
		return nil, validationError("image_unreadable", "image file cannot be read", &image.Index)
	}
	upload := &verifiedUpload{file: temporary, path: temporary.Name()}
	keep := false
	defer func() {
		if !keep {
			_ = upload.Close()
		}
	}()

	written, err := io.Copy(temporary, io.LimitReader(source, MaxFileSize+1))
	if err != nil {
		return nil, validationError("image_unreadable", "image file cannot be read", &image.Index)
	}
	if written > MaxFileSize {
		return nil, validationError("image_too_large", "image file exceeds the 20MB limit", &image.Index)
	}
	contentSHA, mimeType, size, err := inspectImage(temporary, image.Index)
	if err != nil {
		return nil, err
	}
	if contentSHA != image.ContentSHA256 || mimeType != image.MIMEType || size != image.Size {
		return nil, validationError("image_changed", "image changed after preflight", &image.Index)
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return nil, validationError("image_unreadable", "image file cannot be read", &image.Index)
	}
	keep = true
	return upload, nil
}
