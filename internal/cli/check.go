package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newCheckCmd(opts *globalOptions) *cobra.Command {
	var (
		checkType string
		plane     string
		timeout   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "check SRC DST",
		Short: "Run a one-shot connectivity diagnostic from SRC node to DST node",
		Long: `Dispatch a single diagnostic from the agent on the SRC node towards the DST
node, wait for the result, and print a one-line verdict plus the measured
numbers (RTT, loss, jitter — depending on --type).

SRC and DST are Kubernetes node names (as shown by 'kubectl kconmon
topology'). For --type dns and --type http the probe runs the checker's
configured targets from the SRC node and DST is ignored.

The wait is server-side: --timeout is passed to the controller (capped at
120s there); the client allows a small extra margin on top. Exit code is 2
when the check ran to completion but reported failure, so scripts can
distinguish "network broken" (2) from "couldn't ask" (1).`,
		Example: `  # ICMP is the default type
  kubectl kconmon check worker-3 worker-7

  # Specific protocol, longer wait
  kubectl kconmon check worker-3 worker-7 --type udp --timeout 90s

  # DNS health as seen from one node (DST ignored, use any node name)
  kubectl kconmon check worker-3 worker-3 --type dns

  # In scripts: raw JSON on stdout, act on the exit code
  kubectl kconmon check a b --type tcp -o json > result.json || alert`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.validateOutput(); err != nil {
				return err
			}
			req := DiagnosticsRequest{
				Source:      args[0],
				Destination: args[1],
				Type:        checkType,
				Plane:       plane,
			}
			return withClient(cmd.Context(), opts, func(ctx context.Context, c *Client) error {
				res, raw, err := c.Diagnostics(ctx, req, timeout)
				if err != nil {
					return err
				}
				if opts.output == "json" {
					if err := writeJSON(cmd.OutOrStdout(), raw); err != nil {
						return err
					}
				} else if err := formatCheck(cmd.OutOrStdout(), res); err != nil {
					return err
				}
				if !res.Success {
					// Signals exit code 2 without printing anything extra.
					return fmt.Errorf("%s check %s -> %s failed: %w",
						req.Type, req.Source, req.Destination, errCheckFailed)
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&checkType, "type", "icmp", "check type: icmp|tcp|udp|dns|http")
	cmd.Flags().StringVar(&plane, "plane", "pod", "network plane to test")
	cmd.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "diagnostic timeout (controller caps at 120s)")
	return cmd
}

func newMTRCmd(opts *globalOptions) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "mtr SRC DST",
		Short: "Run an MTR-style per-hop trace from SRC node to DST node",
		Long: `Trace the path from the SRC node's agent to the DST node's agent and print
per-hop RTT and loss. Hops that do not answer TTL-expired probes show as
'*' with 100% loss — common for bridges/overlay devices and not by itself a
problem; the verdict is the final hop reaching the destination.

Unlike the automatic MTR that fires when a scheduled probe fails, this
on-demand trace bypasses the per-pair cooldown and runs immediately.`,
		Example: `  kubectl kconmon mtr worker-3 worker-7
  kubectl kconmon mtr worker-3 worker-7 -o json | jq '.details.hops'`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.validateOutput(); err != nil {
				return err
			}
			req := DiagnosticsRequest{
				Source:      args[0],
				Destination: args[1],
				Type:        "mtr",
			}
			return withClient(cmd.Context(), opts, func(ctx context.Context, c *Client) error {
				res, raw, err := c.Diagnostics(ctx, req, timeout)
				if err != nil {
					return err
				}
				if opts.output == "json" {
					if err := writeJSON(cmd.OutOrStdout(), raw); err != nil {
						return err
					}
				} else if err := formatMTR(cmd.OutOrStdout(), res); err != nil {
					return err
				}
				if !res.Success {
					return fmt.Errorf("mtr %s -> %s failed: %w",
						req.Source, req.Destination, errCheckFailed)
				}
				return nil
			})
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "trace timeout (controller caps at 120s)")
	return cmd
}
