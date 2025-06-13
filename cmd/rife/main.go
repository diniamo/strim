package main

import (
	"os"

	log "github.com/diniamo/glog"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("No subcommand")
	}

	switch os.Args[1] {
	case "host":
		runServer(os.Args[2:])
	case "connect":
		runClient(os.Args[2:])
	default:
		log.Fatalf("Invalid subcommand: %s", os.Args[1])
	}
}
