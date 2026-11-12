// Command kx is the Kubernetix command line: kx login, then kx deploy.
package main

import (
	"os"

	"github.com/kubernetix-ai/kx/cmd"
)

func main() { os.Exit(cmd.Main()) }
