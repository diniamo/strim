package main

import (
	"context"
	"errors"

	"github.com/urfave/cli/v3"

	"github.com/diniamo/strim/internal/client"
)

func runClient(ctx context.Context, cmd *cli.Command) error {
	address := cmd.StringArg("address")
	if address == "" {
		return errors.New("Missing address")
	}

	client, err := client.Connect(address)
	if err != nil {
		return errors.New("Connection failed: " + err.Error())
	}

	client.PacketLoop()

	return nil
}
