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
	crop, err := prepareAnnotationCrop(payload, path)
	if err != nil {
		return nil, err
	}
	defer crop.cleanup()
	if err := crop.stage(); err != nil {
		return nil, err
	}
	if err := crop.publish(); err != nil {
		return nil, err
	}
	crop.apply(payload)
	return payload, nil
}

type preparedCrop struct {
	outputPath  string
	destination string
	data        []byte
	temporary   string
}

func prepareAnnotationCrop(payload map[string]json.RawMessage, path string) (*preparedCrop, error) {

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
	destination, err := canonicalCropDestination(path)
	if err != nil {
		return nil, err
	}
	return &preparedCrop{outputPath: path, destination: destination, data: data}, nil
}

func (crop *preparedCrop) apply(payload map[string]json.RawMessage) {
	delete(payload, "cropped_image_base64")
	cropFile, _ := json.Marshal(crop.outputPath)
	payload["crop_file"] = cropFile
}

func (crop *preparedCrop) stage() error {
	temporary, err := os.CreateTemp(filepath.Dir(crop.destination), ".screenote-crop-*")
	if err != nil {
		return genericError("crop_write_failed", "crop file could not be written")
	}
	crop.temporary = temporary.Name()
	keep := true
	defer func() {
		_ = temporary.Close()
		if !keep && crop.temporary != "" {
			_ = os.Remove(crop.temporary)
			crop.temporary = ""
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		keep = false
		return genericError("crop_write_failed", "crop file could not be written")
	}
	if _, err := temporary.Write(crop.data); err != nil {
		keep = false
		return genericError("crop_write_failed", "crop file could not be written")
	}
	if err := temporary.Sync(); err != nil {
		keep = false
		return genericError("crop_write_failed", "crop file could not be written")
	}
	if err := temporary.Close(); err != nil {
		keep = false
		return genericError("crop_write_failed", "crop file could not be written")
	}
	return nil
}

func (crop *preparedCrop) publish() error {
	if crop.temporary == "" {
		return genericError("crop_write_failed", "crop file could not be written")
	}
	if err := rejectSymlinkDestination(crop.destination); err != nil {
		return err
	}
	if err := os.Rename(crop.temporary, crop.destination); err != nil {
		return genericError("crop_write_failed", "crop file could not be written")
	}
	crop.temporary = ""
	return nil
}

func (crop *preparedCrop) cleanup() {
	if crop != nil && crop.temporary != "" {
		_ = os.Remove(crop.temporary)
		crop.temporary = ""
	}
}
