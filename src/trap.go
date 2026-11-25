package main

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var (
	trapMu        sync.Mutex
	trapCallbacks []func()
)

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
	trapMu.Lock()
	trapCallbacks = append(trapCallbacks, callback)
	trapMu.Unlock()
}

func TrapExit(code int) {
	TrapRun()
	os.Exit(code)
}

func TrapRun() {
	trapMu.Lock()

	for i := len(trapCallbacks) - 1; i >= 0; i -= 1 {
		trapCallbacks[i]()
	}
	trapCallbacks = nil

	trapMu.Unlock()
}
