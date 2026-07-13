package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
)

const (
	// Screenote's annotation crop service limits both axes to 1072 pixels.
	maxAnnotationCropDimension = 1072
	maxAnnotationCropPixels    = maxAnnotationCropDimension * maxAnnotationCropDimension
)

func validateCropFilePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", usageError("invalid_crop_file", "--crop-file must name a local output file")
	}
	for _, component := range strings.Split(filepath.ToSlash(value), "/") {
		if component == ".." {
			return "", usageError("invalid_crop_file", "--crop-file must not contain parent traversal")
		}
	}

	path := filepath.Clean(value)
	if path == "." {
		return "", usageError("invalid_crop_file", "--crop-file must name a local output file")
	}
	if _, err := canonicalCropDestination(path); err != nil {
		return "", err
	}
	return path, nil
}

func canonicalCropDestination(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", usageError("invalid_crop_file", "--crop-file is not a valid local path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", usageError("invalid_crop_file", "--crop-file output directory cannot be inspected safely")
	}
	destination := filepath.Join(parent, filepath.Base(absolute))
	if err := rejectSymlinkDestination(destination); err != nil {
		return "", err
	}
	return destination, nil
}

func rejectSymlinkDestination(path string) error {
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return usageError("invalid_crop_file", "--crop-file destination must not be a symlink")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return usageError("invalid_crop_file", "--crop-file destination cannot be inspected safely")
	}
	return nil
}

func exportAnnotationCrop(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		return nil, genericError("invalid_response", "annotation response is not valid JSON")
	}

	encodedRaw, exists := payload["cropped_image_base64"]
	if !exists {
		return nil, genericError("crop_unavailable", "annotation crop is unavailable")
	}
	var encoded *string
	if err := json.Unmarshal(encodedRaw, &encoded); err != nil {
		return nil, genericError("invalid_crop_data", "annotation crop is not valid base64 PNG data")
	}
	if encoded == nil || *encoded == "" {
		return nil, genericError("crop_unavailable", "annotation crop is unavailable")
	}

	data, err := base64.StdEncoding.DecodeString(*encoded)
	if err != nil {
		return nil, genericError("invalid_crop_data", "annotation crop is not valid base64 PNG data")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, genericError("invalid_crop_data", "annotation crop is not valid base64 PNG data")
	}
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxAnnotationCropDimension || config.Height > maxAnnotationCropDimension ||
		config.Width*config.Height > maxAnnotationCropPixels {
		return nil, genericError("crop_too_large", "annotation crop exceeds supported dimensions")
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return nil, genericError("invalid_crop_data", "annotation crop is not valid base64 PNG data")
	}
	if err := writePrivateCrop(path, data); err != nil {
		return nil, err
	}

	delete(payload, "cropped_image_base64")
	cropFile, _ := json.Marshal(path)
	payload["crop_file"] = cropFile
	return payload, nil
}

func writePrivateCrop(path string, data []byte) error {
	destination, err := canonicalCropDestination(path)
	if err != nil {
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(destination), ".screenote-crop-*")
	if err != nil {
		return genericError("crop_write_failed", "crop file could not be written")
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return genericError("crop_write_failed", "crop file could not be written")
	}
	if _, err := temporary.Write(data); err != nil {
		return genericError("crop_write_failed", "crop file could not be written")
	}
	if err := temporary.Sync(); err != nil {
		return genericError("crop_write_failed", "crop file could not be written")
	}
	if err := temporary.Close(); err != nil {
		return genericError("crop_write_failed", "crop file could not be written")
	}
	if err := rejectSymlinkDestination(destination); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return genericError("crop_write_failed", "crop file could not be written")
	}
	keep = true
	return nil
}
