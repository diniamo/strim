package main

import (
	"context"
	"errors"

	log "github.com/diniamo/glog"
	"github.com/diniamo/rife/internal/mpv"
	"github.com/diniamo/rife/internal/server"
	"github.com/urfave/cli/v3"
)

func runServer(ctx context.Context, cmd *cli.Command) error {
	path := cmd.StringArg("path")
	if path == "" {
		return errors.New("Missing path")
	}
	
	mpv, ipc, err := mpv.Open(path)
	if err != nil {
		return errors.New("Failed to open mpv: " + err.Error())
	}

	server := server.NewServer(path, ipc)
	server.RegisterHandlers()
	go server.Listen()

	err = mpv.Wait()
	if err != nil {
		log.Warnf("Mpv exited with an error: %s", err)
	}

	return nil
}
