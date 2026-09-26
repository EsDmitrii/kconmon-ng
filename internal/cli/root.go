package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	// Cloud-provider auth plugins (GKE/EKS/AKS OIDC etc.): without this import
	// client-go cannot authenticate against many managed clusters. See
	// https://krew.sigs.k8s.io/docs/developer-guide/develop/best-practices/
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

// exit codes returned by Execute.
const (
	exitOK    = 0
	exitError = 1
	exitCheck = 2 // check ran but result.success == false
)

// errCheckFailed signals that a diagnostic completed but reported failure, so
// Execute can map it to exit code 2 for scripting.
var errCheckFailed = errors.New("check reported failure")

// globalOptions holds flags shared by every subcommand.
type globalOptions struct {
	output     string
	namespace  string
	kubeconfig string
	context    string
}

// connectorFactory builds the Connector used to reach the controller. It is a
// package variable so tests can swap in a fake that targets an httptest.Server.
var connectorFactory = func(o *globalOptions) Connector {
	return newKubeConnector(o.kubeconfig, o.context, o.namespace)
}

// Execute builds and runs the root command, returning a process exit code.
// The command context is cancelled on SIGINT/SIGTERM so an in-flight request
// and its port-forward teardown are not left to hang indefinitely.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := newRootCmd()
	err := root.ExecuteContext(ctx)
	code := exitCode(err)
	if code == exitError {
		// Cobra already prints usage/flag errors; surface everything else as a
		// single clean line on stderr without Go stack noise.
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
	}
	return code
}

// exitCode maps a command's error to the process exit code.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errCheckFailed):
		return exitCheck
	default:
		return exitError
	}
}

func newRootCmd() *cobra.Command {
	opts := &globalOptions{}

	// When installed via krew the binary is invoked as a kubectl plugin; show
	// the invocation users actually type (krew best practices).
	name := "kubectl-kconmon"
	if strings.HasPrefix(filepath.Base(os.Args[0]), "kubectl-") {
		name = "kconmon"
	}

	root := &cobra.Command{
		Use:   name,
		Short: "Run kconmon-ng connectivity diagnostics from the command line",
		Long: `Talk to the kconmon-ng controller from your terminal: inspect node/agent
topology and run one-shot connectivity checks between any two nodes, without
opening Grafana.

The plugin finds the running kconmon-ng controller pods (labels
app.kubernetes.io/name=kconmon-ng, app.kubernetes.io/component=controller;
every namespace is searched unless -n is given), starts with the Lease holder,
and port-forwards to its http port with your kubeconfig credentials. A
standby's answer moves it to the next replica. The controller dispatches each
check to the agent running on the source node and returns the result. Nothing
is installed or changed in the cluster.

Check types:
  tcp    TCP connect to the destination agent, connect time + total RTT
  udp    UDP packet burst, RTT / jitter / packet loss
  icmp   ICMP echo, RTT + loss
  pmtu   DF-marked UDP datagrams, path MTU verdict (ok|reduced|blackhole)
  dns    resolve the configured hostnames from the source node (DST ignored)
  http   probe the configured HTTP targets from the source node (DST ignored)
  mtr    per-hop trace to the destination, hop-by-hop RTT and loss

Exit codes:
  0  command succeeded (for check/mtr: the probe ran and passed)
  1  CLI or API error (bad arguments, node not found, timeout, no controller)
  2  the check ran to completion but reported failure — useful in scripts

Docs: https://esdmitrii.github.io/kconmon-ng/`,
		Example: `  # Where is everything and are all agents registered?
  kubectl kconmon topology
  kubectl kconmon agents

  # Is UDP healthy between two nodes right now?
  kubectl kconmon check worker-3 worker-7 --type udp

  # Full per-hop trace of a suspicious path
  kubectl kconmon mtr worker-3 worker-7

  # Script-friendly: exit 2 + JSON when the check fails
  kubectl kconmon check worker-3 worker-7 --type icmp -o json || echo "degraded"

  # Non-default namespace, longer server-side wait
  kubectl kconmon check node-a node-b --type tcp -n monitoring --timeout 90s`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&opts.output, "output", "o", "table", "output format: table|json")
	pf.StringVarP(&opts.namespace, "namespace", "n", "", "controller namespace (default: search all namespaces)")
	pf.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: standard client-go rules)")
	pf.StringVar(&opts.context, "context", "", "kubeconfig context to use")

	root.AddCommand(
		newTopologyCmd(opts),
		newAgentsCmd(opts),
		newCheckCmd(opts),
		newMTRCmd(opts),
		newVersionCmd(opts),
	)
	return root
}

// validateOutput ensures the -o value is one we support.
func (o *globalOptions) validateOutput() error {
	switch o.output {
	case "table", "json":
		return nil
	default:
		return fmt.Errorf("invalid output format %q: want table or json", o.output)
	}
}

// withClient opens a connection, invokes fn with a Client, and always closes the connection
// afterward. A standby's answer moves fn to the next controller pod the connection offers: without
// leases RBAC the pod picked first can be the standby.
func withClient(ctx context.Context, opts *globalOptions, fn func(context.Context, *Client) error) error {
	conn, err := connectorFactory(opts).Connect(ctx)
	if err != nil {
		return err
	}
	for {
		err = fn(ctx, NewClient(conn.BaseURL))
		if !isStandbyAnswer(err) || conn.Next == nil {
			conn.Close()
			return err
		}
		next, nerr := conn.Next(ctx)
		conn.Close()
		if nerr != nil {
			return fmt.Errorf("%w; the next controller pod could not be reached: %w", err, nerr)
		}
		conn = next
	}
}
