package main

import (
	"os"
	"os/signal"
	"syscall"
)

var trapCallbacks []func()

func TrapRegister() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		code := 1
		switch <-sigChan {
		case syscall.SIGINT:  code = 130
		case syscall.SIGTERM: code = 143
		}

		TrapExit(code)
	}()
}

func TrapDefer(callback func()) {
	trapCallbacks = append(trapCallbacks, callback)
}

func TrapExit(code int) {
	TrapRun()
	os.Exit(code)
}

func TrapRun() {
	for i := len(trapCallbacks) - 1; i >= 0; i -= 1 {
		trapCallbacks[i]()
	}
}
