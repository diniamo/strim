package main

import (
	"fmt"
	"os"
)

const usageMain = `CLI tool to watch local media with others. Handles both streaming and synchronization.

Usage:
  strim -h/--help
  strim <subcommand...>

Options:
  -h, --help  show this text

Subcommands:
  host, server, h, s  host a server
  connect, client, c  connect to a server
`

func main() {
	switch os.Args[1] {
	case "connect", "client", "c":
		runClient(os.Args[2:])
	case "host", "server", "h", "s":
		runServer(os.Args[2:])
	case "-h", "--help":
		fmt.Print(usageMain)
	}
}
