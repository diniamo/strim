package main

import (
	"context"
	"os"

	log "github.com/diniamo/glog"
	"github.com/urfave/cli/v3"
)

func main() {
	cmd := cli.Command{
		Name: "strim",
		Usage: "a tool to watch local media with others, that handles both streaming and synchronization",
		HideHelpCommand: true,
		Commands: []*cli.Command{
			{
				Name: "host",
				Aliases: []string{"h", "server", "s"},
				Usage: "host a server",
				Arguments: []cli.Argument{
					&cli.StringArg{
						Name: "path",
						UsageText: "the path of the media",
					},
				},
				Action: runServer,
			},
			{
				Name: "connect",
				Aliases: []string{"c", "client"},
				Usage: "connect to a server",
				Arguments: []cli.Argument{
					&cli.StringArg{
						Name: "address",
						UsageText: "the address of the server",
					},
				},
				Action: runClient,
			},
		},
	}

	err := cmd.Run(context.Background(), os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
