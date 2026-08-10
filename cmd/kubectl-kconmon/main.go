// Command kubectl-kconmon is a kubectl plugin that runs connectivity diagnostics against a
// kconmon-ng controller by port-forwarding to its HTTP API.
package main

import (
	"os"

	"github.com/EsDmitrii/kconmon-ng/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
