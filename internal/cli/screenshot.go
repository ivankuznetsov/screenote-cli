package cli

import (
	"mime"
	"os"
	"path/filepath"

	"github.com/ivankuznetsov/screenote-cli/internal/screenote"
	"github.com/spf13/cobra"
)

func (a *app) screenshotCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "screenshot", Short: "Screenshot commands", Args: rejectArgs, RunE: showHelp}

	var listPage, listStatus string
	var limit, offset int
	list := &cobra.Command{
		Use:   "list",
		Short: "List screenshots",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, project, err := a.clientForProject(cmd.Context())
			if err != nil {
				return err
			}
			query := screenote.WithLimitOffset(screenote.Query(map[string]string{
				"page_id": listPage,
				"status":  listStatus,
			}), limit, offset)
			raw, _, err := client.Screenshots(cmd.Context(), project, query)
			if err != nil {
				return err
			}
			return writeRawJSON(a.stdout, raw)
		},
	}
	list.Flags().StringVar(&listPage, "page", "", "Page ID")
	list.Flags().StringVar(&listStatus, "status", "", "Screenshot status")
	list.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	list.Flags().IntVar(&offset, "offset", 0, "Results to skip")

	var title, pageValue, filePath string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a screenshot",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if title == "" {
				return missingFlag("title")
			}
			client, project, err := a.clientForProject(cmd.Context())
			if err != nil {
				return err
			}

			reader := a.stdin
			filename := "stdin"
			contentType := ""
			if filePath != "" && filePath != "-" {
				file, err := os.Open(filePath)
				if err != nil {
					return err
				}
				defer file.Close()
				reader = file
				filename = filePath
				contentType = mime.TypeByExtension(filepath.Ext(filePath))
			}

			raw, err := client.CreateScreenshot(cmd.Context(), project, title, pageValue, filename, contentType, reader)
			if err != nil {
				return err
			}
			return writeRawJSON(a.stdout, raw)
		},
	}
	create.Flags().StringVar(&title, "title", "", "Screenshot title")
	create.Flags().StringVar(&pageValue, "page", "", "Page ID or page name")
	create.Flags().StringVar(&filePath, "file", "-", "Image file path or - for stdin")

	cmd.AddCommand(list, create)
	return cmd
}
