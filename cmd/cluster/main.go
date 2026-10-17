// Command cluster bridges a Slurm cluster and your workstation.
package main

import (
	"os"

	"github.com/lunar-photon/cluster/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
