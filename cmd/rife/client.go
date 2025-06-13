package main

import (
	log "github.com/diniamo/glog"

	"github.com/diniamo/rife/internal/client"
)

func runClient(args []string) {
	if len(args) < 1 {
		log.Fatal("Missing server address")
	}
	address := args[0]

	client, err := client.Connect(address)
	if err != nil {
		log.Fatalf("Connection failed: %s", err)
	}

	client.PacketLoop()
}
