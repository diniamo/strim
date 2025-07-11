package main

import (
	"errors"
	"fmt"
	"os"

	log "github.com/diniamo/glog"
	"github.com/diniamo/strim/internal/client"
	"github.com/diniamo/strim/internal/mpv"
)

const usageClient = `Connect to a server.

Usage:
  strim <connect/client/c> [-h/--help] <server address> [mpv arguments...]

Options:
  -h, --help  show this text
`

func runClient(args []string) error {
	if len(args) < 1 {
		fmt.Print(usageClient)
		return errors.New("Not enough arguments")
	}

	if args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usageClient)
		return nil
	}
	

	address := args[0]
	
	mpv, ipc, err := mpv.Open(append(
		args[1:],
		"--idle", "--force-window",
		"--no-resume-playback", "--no-save-position-on-quit",
		"--pause",
	)...)
	if err != nil {
		return errors.New("Failed to open mpv: " + err.Error())
	}
	go func() {
		err := mpv.Wait()
		if err == nil {
			os.Exit(0)
		} else {
			log.Warnf("Mpv exited with an error: %s", err)
			os.Exit(1)
		}
	}()

	client := client.New(ipc, address)
	
	err = client.Connect()
	if err != nil {
		return errors.New("Connection failed: " + err.Error())
	}

	client.RegisterHandlers()
	client.PacketLoop()

	return nil
}
