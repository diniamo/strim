package main

import (
	"fmt"
	"os"

	"github.com/LordOfTrident/ansi-go"
)

const usageText = `CLI tool to watch local media with others. Handles both streaming and synchronization.

Usage:
  strim
  strim host [mpv arguments...]
  strim connect [phrase [mpv arguments...]]
  strim -h/--help
`

func main() {
	var verb string
	if len(os.Args) > 1 {
		verb = os.Args[1]
	} else {
		Print("Do you want to ", ansi.Bold, "h", ansi.Reset, "ost or ", ansi.Bold, "c", ansi.Reset, "onnect? ")
		answer := readAnswer()

		switch answer {
		case 'h', 'H': verb = "host"
		case 'c', 'C': verb = "connect"
		default:       Fatal("Invalid answer")
		}
	}

	var args []string
	if len(os.Args) > 2 {
		args = os.Args[2:]
	}

	switch verb {
	case "host":
		runServer(args)
	case "connect":
		runClient(args)
	case "-h", "--help":
		usage(0)
	default:
		usage(1)
	}
}

func usage(code int) {
	fmt.Fprint(os.Stderr, usageText)
	os.Exit(code)
}
