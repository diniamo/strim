package main

import (
	"fmt"

	log "github.com/diniamo/glog"
	"github.com/diniamo/strim/internal/mpv"
	"github.com/diniamo/strim/internal/server"
)

const usageServer = `Host a server.

At least 1 mpv argument must be specified, which is the file to play.

Usage:
  strim <host/server/h/s> [-h/--help] <mpv arguments...>

Options:
  -h, --help  show this text
`

func runServer(args []string) {
	if len(args) < 1 {
		fmt.Print(usageServer)
		log.Fatal("Not enough arguments")
	}

	if args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usageServer)
		return
	}
	

	mpv, ipc, err := mpv.Open(args...)
	if err != nil {
		log.Fatalf("Failed to open mpv: %s", err)
	}

	server := server.New(ipc)
	server.RegisterHandlers()
	go server.ListenAndServe()

	err = mpv.Wait()
	if err != nil {
		log.Warnf("Mpv exited with an error: %s", err)
	}
}
