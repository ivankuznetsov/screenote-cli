package cli

import "github.com/spf13/cobra"

func (a *app) commentCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "comment", Short: "Comment commands", Args: rejectArgs, RunE: showHelp}

	var annotationID, body string
	var image singleImageFlag
	add := &cobra.Command{
		Use:   "add",
		Short: "Add an annotation comment",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if annotationID == "" {
				return missingFlag("annotation")
			}
			if body == "" {
				return missingFlag("body")
			}
			if image.count > 1 {
				return usageError("invalid_image", "--image may only be specified once")
			}
			if image.count == 1 {
				return a.addImageComment(cmd.Context(), annotationID, body, image.value)
			}
			client, project, err := a.clientForProject(cmd.Context())
			if err != nil {
				return err
			}
			raw, err := client.AddComment(cmd.Context(), annotationID, project, body)
			if err != nil {
				return err
			}
			return writeRawJSON(a.stdout, raw)
		},
	}
	add.Flags().StringVar(&annotationID, "annotation", "", "Annotation ID")
	add.Flags().StringVar(&body, "body", "", "Comment body")
	add.Flags().Var(&image, "image", "Attach one PNG, JPEG, or WebP image from a local path or - for stdin")
	cmd.AddCommand(add)
	return cmd
}
