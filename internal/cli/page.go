package cli

import "github.com/spf13/cobra"

func (a *app) pageCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "page", Short: "Page commands", Args: rejectArgs, RunE: showHelp}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List pages",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, project, err := a.clientForProject(cmd.Context())
			if err != nil {
				return err
			}
			raw, err := client.Pages(cmd.Context(), project)
			if err != nil {
				return err
			}
			return writeRawJSON(a.stdout, raw)
		},
	})
	return cmd
}
