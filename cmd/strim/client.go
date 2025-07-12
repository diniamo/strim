package main

import (
	"fmt"

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

func runClient(args []string) {
	if len(args) < 1 {
		fmt.Print(usageClient)
		log.Fatal("Not enough arguments")
	}

	if args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usageClient)
		return
	}
	

	address := args[0]
	
	mpv, ipc, err := mpv.Open(append(
		args[1:],
		"--idle", "--force-window",
		"--no-resume-playback", "--no-save-position-on-quit",
		"--pause",
	)...)
	if err != nil {
		log.Fatalf("Failed to open mpv: %s", err)
	}

	mpvExitChan := make(chan struct{})
	go func() {
		err := mpv.Wait()
		if err != nil {
			log.Warnf("Mpv exited with an error: %s", err)
		}

		mpvExitChan <- struct{}{}
	}()

	client := client.New(ipc, address)
	
	err = client.Connect()
	if err != nil {
		log.Fatalf("Connection failed: %s", err)
	}
	
	log.Success("Connection established")

	client.RegisterHandlers()
	client.PacketLoop()

	_, err = ipc.RequestSync("quit")
	if err == nil {
		<-mpvExitChan
	} else {
		log.Warnf("Failed to quit mpv: %s", err)
	}
}
