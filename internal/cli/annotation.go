package cli

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
	"github.com/spf13/cobra"
)

// pageSize is the maximum number of records the REST API returns per request.
const pageSize = 100

func (a *app) annotationCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "annotation", Short: "Annotation commands", Args: rejectArgs, RunE: showHelp}

	var screenshotID, status, viewport string
	var limit, offset int
	list := &cobra.Command{
		Use:   "list",
		Short: "List annotations",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, project, err := a.clientForProject(cmd.Context())
			if err != nil {
				return err
			}
			filters := screenote.Query(map[string]string{
				"status":   status,
				"viewport": viewport,
			})
			if screenshotID != "" {
				raw, _, err := client.Annotations(cmd.Context(), screenshotID, project, screenote.WithLimitOffset(cloneValues(filters), limit, offset))
				if err != nil {
					return err
				}
				return writeRawJSON(a.stdout, raw)
			}

			screenshots, err := allScreenshots(cmd.Context(), client, project)
			if err != nil {
				return err
			}
			// Aggregate every annotation across every screenshot first, so
			// --limit/--offset and the reported total describe the whole set
			// rather than a single per-screenshot slice.
			annotations := make([]screenote.Annotation, 0)
			for _, screenshot := range screenshots {
				response, err := allAnnotations(cmd.Context(), client, intString(screenshot.ID), project, filters)
				if err != nil {
					// A screenshot deleted between the list and the per-screenshot
					// fetch returns 404; skip it and keep aggregating the rest.
					// Every other failure (auth, rate limit, 5xx, network) must
					// propagate so the listing never seals a partial result as
					// complete.
					var apiErr *screenote.Error
					if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
						continue
					}
					return err
				}
				annotations = append(annotations, response...)
			}
			total := len(annotations)
			return writeJSON(a.stdout, map[string]any{
				"annotations": pageAnnotations(annotations, limit, offset),
				"pagination": map[string]int{
					"total":  total,
					"limit":  limit,
					"offset": offset,
				},
			})
		},
	}
	list.Flags().StringVar(&screenshotID, "screenshot", "", "Screenshot ID")
	list.Flags().StringVar(&status, "status", "", "Annotation status")
	list.Flags().StringVar(&viewport, "viewport", "", "Viewport")
	list.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	list.Flags().IntVar(&offset, "offset", 0, "Results to skip")

	var annotationID, cropFile string
	get := &cobra.Command{
		Use:   "get",
		Short: "Get annotation details",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if annotationID == "" {
				return missingFlag("annotation")
			}
			if cropFile != "" {
				var err error
				cropFile, err = validateCropFilePath(cropFile)
				if err != nil {
					return err
				}
			}
			client, project, err := a.clientForProject(cmd.Context())
			if err != nil {
				return err
			}
			raw, err := client.Annotation(cmd.Context(), annotationID, project)
			if err != nil {
				return err
			}
			if cropFile != "" {
				payload, err := exportAnnotationCrop(raw, cropFile)
				if err != nil {
					return err
				}
				return writeJSON(a.stdout, payload)
			}
			return writeRawJSON(a.stdout, raw)
		},
	}
	get.Flags().StringVar(&annotationID, "annotation", "", "Annotation ID")
	get.Flags().StringVar(&cropFile, "crop-file", "", "Write the annotation crop to a private local PNG file")

	var resolveAnnotationID, comment string
	resolve := &cobra.Command{
		Use:   "resolve",
		Short: "Resolve an annotation",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if resolveAnnotationID == "" {
				return missingFlag("annotation")
			}
			client, project, err := a.clientForProject(cmd.Context())
			if err != nil {
				return err
			}
			raw, err := client.ResolveAnnotation(cmd.Context(), resolveAnnotationID, project, comment)
			if err != nil {
				return err
			}
			return writeRawJSON(a.stdout, raw)
		},
	}
	resolve.Flags().StringVar(&resolveAnnotationID, "annotation", "", "Annotation ID")
	resolve.Flags().StringVar(&comment, "comment", "", "Resolution comment")

	cmd.AddCommand(list, get, resolve)
	return cmd
}

// allScreenshots pages through every screenshot in a project so the aggregate
// annotation listing is never silently capped at the first page.
func allScreenshots(ctx context.Context, client *screenote.Client, project string) ([]screenote.Screenshot, error) {
	all := make([]screenote.Screenshot, 0)
	for offset := 0; ; offset += pageSize {
		_, response, err := client.Screenshots(ctx, project, screenote.WithLimitOffset(screenote.Query(nil), pageSize, offset))
		if err != nil {
			return nil, err
		}
		all = append(all, response.Screenshots...)
		if len(response.Screenshots) < pageSize {
			return all, nil
		}
	}
}

// allAnnotations pages through every annotation for a single screenshot,
// applying the shared status/viewport filters.
func allAnnotations(ctx context.Context, client *screenote.Client, screenshot, project string, filters url.Values) ([]screenote.Annotation, error) {
	all := make([]screenote.Annotation, 0)
	for offset := 0; ; offset += pageSize {
		_, response, err := client.Annotations(ctx, screenshot, project, screenote.WithLimitOffset(cloneValues(filters), pageSize, offset))
		if err != nil {
			return nil, err
		}
		all = append(all, response.Annotations...)
		if len(response.Annotations) < pageSize {
			return all, nil
		}
	}
}

// pageAnnotations applies the aggregate --limit/--offset window in memory.
func pageAnnotations(items []screenote.Annotation, limit, offset int) []screenote.Annotation {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(items) {
		return []screenote.Annotation{}
	}
	items = items[offset:]
	if limit > 0 && limit < len(items) {
		items = items[:limit]
	}
	return items
}

// cloneValues copies query values so per-request limit/offset mutations do not
// leak into the shared filter set.
func cloneValues(values url.Values) url.Values {
	out := url.Values{}
	for key, vals := range values {
		out[key] = append([]string(nil), vals...)
	}
	return out
}
