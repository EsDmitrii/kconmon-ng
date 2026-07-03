package cli

import (
	"context"

	"github.com/spf13/cobra"
)

func newTopologyCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "topology",
		Short: "Show nodes, zones, and agent registration state",
		Long: `Print the controller's view of the cluster: every node with its zone and
readiness, and the kconmon-ng agent registered on it (name and pod IP).
A node without an agent row means probes are not running from that node —
check the DaemonSet.`,
		Example: `  kubectl kconmon topology
  kubectl kconmon topology -o json | jq '.nodes[] | select(.ready==false)'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.validateOutput(); err != nil {
				return err
			}
			return withClient(cmd.Context(), opts, func(ctx context.Context, c *Client) error {
				snap, raw, err := c.Topology(ctx)
				if err != nil {
					return err
				}
				if opts.output == "json" {
					return writeJSON(cmd.OutOrStdout(), raw)
				}
				return formatTopology(cmd.OutOrStdout(), snap)
			})
		},
	}
}

func newAgentsCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "agents",
		Short: "List registered agents",
		Long: `List every agent currently registered with the controller: agent ID, node,
pod IP, zone, and how long ago its last heartbeat was seen. An agent whose
LAST SEEN keeps growing is about to be evicted (heartbeat TTL, default 30s).`,
		Example: `  kubectl kconmon agents
  kubectl kconmon agents -n monitoring -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.validateOutput(); err != nil {
				return err
			}
			return withClient(cmd.Context(), opts, func(ctx context.Context, c *Client) error {
				snap, raw, err := c.Topology(ctx)
				if err != nil {
					return err
				}
				if opts.output == "json" {
					return writeJSON(cmd.OutOrStdout(), raw)
				}
				return formatAgents(cmd.OutOrStdout(), snap)
			})
		},
	}
}
