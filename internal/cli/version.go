package cli

import "github.com/spf13/cobra"

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print CLI build information",
		Args:  rejectArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeJSON(a.stdout, struct {
				Version   string `json:"version"`
				Commit    string `json:"commit"`
				BuildDate string `json:"build_date"`
			}{
				Version:   Version,
				Commit:    Commit,
				BuildDate: BuildDate,
			})
		},
	}
}
