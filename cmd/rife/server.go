package main

import (
	"os"

	log "github.com/diniamo/glog"
	"github.com/diniamo/rife/internal/mpv"
	"github.com/diniamo/rife/internal/server"
)

func runServer(args []string) {
	if len(args) < 1 {
		log.Fatal("No input file")
	}
	path := args[0]

	mpv, ipc, err := mpv.Open(path)
	if err != nil {
		log.Fatalf("Failed to open mpv: %s", err)
	}

	server := server.NewServer(path, ipc)
	server.RegisterHandlers()
	go server.Listen()

	err = mpv.Wait()
	if err != nil {
		log.Warnf("Mpv exited with an error: %s", err)
		os.Exit(1)
	}
}
