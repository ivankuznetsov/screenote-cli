package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"time"

	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
	snapshotmanifest "github.com/ivankuznetsov/screenote-cli/internal/snapshot"
	"github.com/spf13/cobra"
)

const (
	defaultSnapshotWait = 2 * time.Minute
	maxSnapshotWait     = 30 * time.Minute
	defaultPollInterval = time.Second
	maxPollInterval     = 5 * time.Second
)

func (a *app) snapshotCommand() *cobra.Command {
	var manifestPath string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Publish a manifest-driven snapshot",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if manifestPath == "" {
				return missingFlag("manifest")
			}
			if wait <= 0 || wait > maxSnapshotWait {
				return usageError("invalid_wait", "--wait must be greater than zero and no more than 30m")
			}

			prepared, err := snapshotmanifest.Load(manifestPath)
			if err != nil {
				return manifestPreflightError(err)
			}
			client, project, err := a.clientForProject(cmd.Context())
			if err != nil {
				return err
			}
			return a.publishSnapshot(cmd.Context(), client, project, prepared, wait)
		},
	}
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "Path to a snapshot manifest")
	cmd.Flags().DurationVar(&wait, "wait", defaultSnapshotWait, "Maximum time to wait for image processing")
	return cmd
}

func manifestPreflightError(err error) error {
	var validation *snapshotmanifest.ValidationError
	if errors.As(err, &validation) {
		return withOperation(usageError(validation.Code, validation.Message), "preflight", validation.Index, 0)
	}
	return withOperation(usageError("invalid_manifest", "manifest preflight failed"), "preflight", nil, 0)
}

func (a *app) publishSnapshot(ctx context.Context, client *screenote.Client, project string, prepared *snapshotmanifest.PreparedManifest, wait time.Duration) error {
	request := screenote.SnapshotPrepareRequest{
		Version:        prepared.Version,
		GitCommit:      prepared.GitCommit,
		TakenAt:        prepared.TakenAt,
		ManifestDigest: prepared.ManifestDigest,
		Entries:        make([]screenote.SnapshotPrepareEntry, 0, len(prepared.Images)),
	}
	for _, image := range prepared.Images {
		request.Entries = append(request.Entries, screenote.SnapshotPrepareEntry{
			Page: image.Page, Title: image.Title, Viewport: image.Viewport,
			MIMEType: image.MIMEType, ContentSHA256: image.ContentSHA256, FileRefSHA256: image.FileRefSHA256,
		})
	}

	response, err := client.PrepareSnapshot(ctx, project, request)
	if err != nil {
		return withOperation(err, "prepare_snapshot", nil, 0)
	}
	if response.ManifestDigest != prepared.ManifestDigest {
		return withOperation(genericError("manifest_conflict", "server returned a different manifest identity"), "prepare_snapshot", nil, response.SnapshotID)
	}
	if err := writeJSON(a.stdout, map[string]any{
		"event": "snapshot_prepared", "operation": response.Operation,
		"snapshot_id": response.SnapshotID, "manifest_digest": response.ManifestDigest, "state": response.State,
	}); err != nil {
		return err
	}

	remoteEntries := make(map[string]screenote.SnapshotEntryResponse, len(response.Entries))
	for _, entry := range response.Entries {
		remoteEntries[snapshotKey(entry.ManifestEntryDigest, entry.Viewport)] = entry
	}
	for index, image := range prepared.Images {
		remote, ok := remoteEntries[snapshotKey(image.GroupDigest, image.Viewport)]
		if !ok || remote.ContentSHA256 != image.ContentSHA256 || remote.MIMEType != image.MIMEType {
			entry := index
			return withOperation(genericError("invalid_response", "server response does not match the prepared manifest"), "prepare_snapshot", &entry, response.SnapshotID)
		}
		if remote.Attached && remote.State != "failed" {
			if err := writeJSON(a.stdout, map[string]any{
				"event": "image_skipped", "operation": "already_uploaded", "snapshot_id": response.SnapshotID,
				"manifest_entry": index, "image_id": remote.ImageID, "viewport": image.Viewport,
			}); err != nil {
				return err
			}
			continue
		}

		file, err := image.OpenVerified()
		if err != nil {
			entry := index
			return withOperation(manifestPreflightError(err), "upload_image", &entry, response.SnapshotID)
		}
		uploaded, uploadErr := client.UploadSnapshotImage(ctx, project, remote.ImageID, image.MIMEType, image.Size, file)
		_ = file.Close()
		if uploadErr != nil {
			entry := index
			return withOperation(uploadErr, "upload_image", &entry, response.SnapshotID)
		}
		if uploaded.SnapshotID != response.SnapshotID || uploaded.ImageID != remote.ImageID || !uploaded.Attached || !validSnapshotState(uploaded.SnapshotState) {
			entry := index
			return withOperation(genericError("invalid_response", "image upload response does not match the prepared snapshot"), "upload_image", &entry, response.SnapshotID)
		}
		snapshotStateChanged := uploaded.SnapshotState != response.State
		if err := writeJSON(a.stdout, map[string]any{
			"event": "image_uploaded", "operation": uploaded.Operation, "snapshot_id": response.SnapshotID,
			"manifest_entry": index, "image_id": remote.ImageID, "viewport": image.Viewport, "state": uploaded.State,
		}); err != nil {
			return err
		}
		if snapshotStateChanged {
			if err := writeJSON(a.stdout, map[string]any{
				"event": "snapshot_state", "snapshot_id": response.SnapshotID, "state": uploaded.SnapshotState,
			}); err != nil {
				return err
			}
		}
		response.State = uploaded.SnapshotState
	}

	return a.waitForSnapshot(ctx, client, project, response, wait)
}

func (a *app) waitForSnapshot(ctx context.Context, client *screenote.Client, project string, initial screenote.SnapshotResponse, wait time.Duration) error {
	if initial.State == "ready" {
		return a.writeSnapshotReady(initial)
	}
	if initial.State == "failed" {
		return withOperation(genericError("snapshot_failed", "snapshot image processing failed"), "wait_for_snapshot", nil, initial.SnapshotID)
	}

	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	interval := a.snapshotPollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	timer := time.NewTimer(a.snapshotPollDelay(interval))
	defer timer.Stop()
	lastState := initial.State
	var lastPollError error

	for {
		select {
		case <-ctx.Done():
			return withOperation(ctx.Err(), "wait_for_snapshot", nil, initial.SnapshotID)
		case <-waitCtx.Done():
			if lastPollError != nil {
				return withOperation(lastPollError, "get_snapshot", nil, initial.SnapshotID)
			}
			return withOperation(genericError("snapshot_timeout", "snapshot is still processing; rerun the unchanged manifest to resume"), "wait_for_snapshot", nil, initial.SnapshotID)
		case <-timer.C:
			response, err := client.Snapshot(waitCtx, project, initial.SnapshotID)
			if err != nil {
				if ctx.Err() != nil {
					return withOperation(ctx.Err(), "wait_for_snapshot", nil, initial.SnapshotID)
				}
				if waitCtx.Err() != nil {
					return withOperation(genericError("snapshot_timeout", "snapshot is still processing; rerun the unchanged manifest to resume"), "wait_for_snapshot", nil, initial.SnapshotID)
				}
				if retryableSnapshotPollError(err) {
					lastPollError = err
					interval = nextSnapshotPollInterval(interval)
					timer.Reset(a.snapshotPollDelay(interval))
					continue
				}
				return withOperation(err, "get_snapshot", nil, initial.SnapshotID)
			}
			lastPollError = nil
			if response.State != lastState {
				if err := writeJSON(a.stdout, map[string]any{
					"event": "snapshot_state", "snapshot_id": response.SnapshotID, "state": response.State,
				}); err != nil {
					return err
				}
				lastState = response.State
				interval = a.snapshotPollInterval
				if interval <= 0 {
					interval = defaultPollInterval
				}
			} else {
				interval = nextSnapshotPollInterval(interval)
			}
			switch response.State {
			case "ready":
				return a.writeSnapshotReady(response)
			case "failed":
				return withOperation(genericError("snapshot_failed", "snapshot image processing failed"), "wait_for_snapshot", nil, response.SnapshotID)
			}
			timer.Reset(a.snapshotPollDelay(interval))
		}
	}
}

func validSnapshotState(state string) bool {
	switch state {
	case "awaiting_upload", "processing", "failed", "ready":
		return true
	default:
		return false
	}
}

func retryableSnapshotPollError(err error) bool {
	var apiError *screenote.Error
	if errors.As(err, &apiError) {
		return apiError.StatusCode == 429 || apiError.StatusCode >= 500
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func nextSnapshotPollInterval(interval time.Duration) time.Duration {
	if interval >= maxPollInterval {
		return maxPollInterval
	}
	interval *= 2
	if interval > maxPollInterval {
		return maxPollInterval
	}
	return interval
}

func (a *app) snapshotPollDelay(interval time.Duration) time.Duration {
	if a.snapshotPollJitter != nil {
		return a.snapshotPollJitter(interval)
	}
	spread := interval / 5
	if spread <= 0 {
		return interval
	}
	offset := time.Duration(rand.Int64N(int64(spread)*2+1)) - spread
	return interval + offset
}

func (a *app) writeSnapshotReady(response screenote.SnapshotResponse) error {
	if response.ReviewURL == "" {
		return withOperation(genericError("invalid_response", "ready snapshot response is missing review_url"), "get_snapshot", nil, response.SnapshotID)
	}
	return writeJSON(a.stdout, map[string]any{
		"event": "snapshot_ready", "snapshot_id": response.SnapshotID,
		"state": response.State, "review_url": response.ReviewURL,
	})
}

func snapshotKey(groupDigest, viewport string) string {
	return fmt.Sprintf("%s\x00%s", groupDigest, viewport)
}
