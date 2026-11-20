package cmd

import (
	"github.com/spf13/cobra"

	"github.com/kubernetix-ai/kx/internal/version"
)

func init() { register(newVersionCmd) }

func newVersionCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Show which kx this is",
		GroupID: groupMore,
		Example: `  kx version
  kx version --json`,
		Args: usageArgs(0, 0, ""),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := a.printer()
			if a.jsonOut {
				return p.PrintJSON(version.Get())
			}
			p.Result(version.String())
			return nil
		},
	}
}
