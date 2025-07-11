package main

import (
	"errors"
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

func runServer(args []string) error {
	if len(args) < 1 {
		fmt.Print(usageServer)
		return errors.New("Not enough arguments")
	}

	if args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usageServer)
		return nil
	}
	

	mpv, ipc, err := mpv.Open(args...)
	if err != nil {
		return errors.New("Failed to open mpv: " + err.Error())
	}

	server := server.New(ipc)
	server.RegisterHandlers()
	go server.ListenAndServe()

	err = mpv.Wait()
	if err != nil {
		log.Warnf("Mpv exited with an error: %s", err)
	}

	return nil
}
