package cli

import (
	appconfig "github.com/ivankuznetsov/screenote-cli/internal/config"
	"github.com/spf13/cobra"
)

func (a *app) configCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print resolved configuration",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := a.resolvedConfig()
			if err != nil {
				return err
			}
			return writeJSON(a.stdout, struct {
				BaseURL  string            `json:"base_url,omitempty"`
				Project  string            `json:"project,omitempty"`
				TokenSet bool              `json:"token_set"`
				Sources  appconfig.Sources `json:"sources"`
			}{
				BaseURL:  resolved.BaseURL,
				Project:  resolved.Project,
				TokenSet: resolved.Token != "",
				Sources:  resolved.Sources,
			})
		},
	}

	var setValues appconfig.Values
	set := &cobra.Command{
		Use:   "set",
		Short: "Write configuration values",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := defaultConfigPath(a.configPath)
			values, err := appconfig.LoadExpanded(path)
			if err != nil {
				return err
			}
			if setValues.Token != "" {
				values.Token = setValues.Token
			}
			if setValues.BaseURL != "" {
				values.BaseURL = setValues.BaseURL
			}
			if setValues.Project != "" {
				values.Project = setValues.Project
			}
			if err := appconfig.Save(path, values); err != nil {
				return err
			}
			return writeJSON(a.stdout, map[string]any{"ok": true, "path": path})
		},
	}
	set.Flags().StringVar(&setValues.Token, "token", "", "OAuth bearer token to write")
	set.Flags().StringVar(&setValues.BaseURL, "base-url", "", "Base URL to write")
	set.Flags().StringVar(&setValues.Project, "project", "", "Project ID to write")
	cmd.AddCommand(set)

	return cmd
}
