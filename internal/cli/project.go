package cli

import "github.com/spf13/cobra"

func (a *app) projectCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Project commands", Args: rejectArgs, RunE: showHelp}

	var name string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a project",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return missingFlag("name")
			}
			client, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			raw, err := client.CreateProject(cmd.Context(), name)
			if err != nil {
				return err
			}
			return writeRawJSON(a.stdout, raw)
		},
	}
	create.Flags().StringVar(&name, "name", "", "Project name")

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List projects",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := a.client(cmd.Context())
			if err != nil {
				return err
			}
			raw, _, err := client.Projects(cmd.Context())
			if err != nil {
				return err
			}
			return writeRawJSON(a.stdout, raw)
		},
	})
	cmd.AddCommand(create)
	return cmd
}
