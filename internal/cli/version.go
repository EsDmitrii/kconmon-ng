package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/spf13/cobra"
)

func newVersionCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show client and controller version",
		Long: `Print the plugin's own version and the version reported by the controller
in the cluster. A version skew is usually fine — the diagnostics API is
additive — but worth knowing when something misbehaves.`,
		Example: `  kubectl kconmon version`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.validateOutput(); err != nil {
				return err
			}
			return withClient(cmd.Context(), opts, func(ctx context.Context, c *Client) error {
				srv, _, err := c.Version(ctx)
				if err != nil {
					return err
				}
				if opts.output == "json" {
					out := map[string]any{
						"client":     clientVersion(),
						"controller": srv,
					}
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(out)
				}
				w := cmd.OutOrStdout()
				if _, werr := fmt.Fprintf(w, "Client:     %s (commit %s)\n", config.Version, config.Commit); werr != nil {
					return werr
				}
				_, err = fmt.Fprintf(w, "Controller: %s (commit %s)\n", orDash(srv.Version), orDash(srv.Commit))
				return err
			})
		},
	}
}

// clientVersion returns the client build info in the same shape as the
// controller version block.
func clientVersion() VersionInfo {
	return VersionInfo{Version: config.Version, Commit: config.Commit}
}
