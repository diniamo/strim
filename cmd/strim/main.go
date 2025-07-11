package main

import (
	"fmt"
	"os"

	log "github.com/diniamo/glog"
)

const usageMain = `A tool to watch local media with others. Handles both streaming and synchronization.

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
	var err error

	switch os.Args[1] {
	case "connect", "client", "c":
		err = runClient(os.Args[2:])
	case "host", "server", "h", "s":
		err = runServer(os.Args[2:])
	case "-h", "--help":
		fmt.Print(usageMain)
	}
	
	if err != nil {
		log.Fatal(err)
	}
}
