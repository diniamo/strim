package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/LordOfTrident/ansi-go"
	"github.com/diniamo/gopv"
	"github.com/tailscale/tailcat"
	"golang.design/x/clipboard"
	"tailscale.com/types/logger"
)

type client struct {
	// The message connection.
	messageConn net.Conn
	// The channel used for incoming messages.
	receiveChan chan message

	// The mpv process.
	mpv *exec.Cmd
	// The IPC client.
	ipc *gopv.Client
	// The debouncer used for ignoring events that were caused by our own requests.
	debouncer debouncer

	// Whether the session is currently waiting.
	// All media controls should be blocked, except speed.
	waiting bool
	// The time at which the wait started.
	// Used for preventing unpauses by seeking back to here.
	waitTime float64
	// Whether a ready message should be sent on the next playback-restart event.
	sendReady bool

	// Whether the user requested that the media player or strim exit.
	// Errors are ignored if this is set.
	shuttingDown atomic.Bool
}

const (
	addressPrompt = ansi.Bold + "Server address" + ansi.Reset + ": "
	dialTimeout   = 20 * time.Second
)

func runClient(args []string) {
	// SECTION: trap setup
	TrapRegister()
	defer TrapRun()

	// SECTION: clipboard setup
	err := clipboard.Init()
	if err != nil { Warn("Failed to initialize clipboard:", err) }

	// SECTION: client data
	c := &client{
		receiveChan: make(chan message),
	}

	// SECTION: input
	var address string
	var mpvArgs []string
	if len(args) > 0 {
		address = args[0]
		mpvArgs = args[1:]
	} else {
		bytes := clipboard.Read(clipboard.FmtText)
		if bytes != nil {
			address = string(bytes)
		}

		if isValidAddress(address) {
			Note("Using address from clipboard")
		} else {
			address = readLine(addressPrompt)
		}
	}

	for {
		address = strings.TrimSpace(address)
		if isValidAddress(address) { break }
		Error("Invalid address")
		address = readLine(addressPrompt)
	}

	// SECTION: connection
	tc := &tailcat.Client{
		Server: tailcat.Addr(address),
		Logf: logger.Discard,
	}
	TrapDefer(func() {
		tc.Close()
	})

	dialCtx, dialCancel := context.WithTimeout(context.Background(), dialTimeout)
	c.messageConn, err = tc.DialTCPPort(dialCtx, messagePort)
	dialCancel()
	if err != nil { Fatal("Failed to connect to server:", err) }
	Success("Connected")

	TrapDefer(func() {
		c.messageConn.Close()
	})

	go func() {
		var data [maxMessageSize]byte

		for {
			// NOTE: nothing references data in the payload,
			// because numbers obviously don't, and string(...) creates a copy in Go.
			typ, payload, err := waitMessage(c.messageConn, data[:])
			if err != nil {
				c.receiveChan <- message{messageTypeCount, err}
				close(c.receiveChan)
				break
			}

			c.receiveChan <- message{typ, payload}
		}
	}()

	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { Fatal("Failed to start proxy listener:", err) }
	proxyPort := strconv.Itoa(proxyListener.Addr().(*net.TCPAddr).Port)

	go func() {
		proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "strim"})
		proxy.ErrorLog = log.New(io.Discard, "", 0)
		proxy.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return tc.DialTCPPort(ctx, streamPort)
			},
		}

		err := http.Serve(proxyListener, proxy)
		if err != nil { Fatal("Failed to serve proxy:", err) }
	}()

	// SECTION: mpv
	setupPath()

	c.mpv, c.ipc = mpv(append(mpvArgs,
		// IMPORTANT: imprecise seeks may result in multiple seconds of desync,
		// since what would be the precise seek is reported anyway
		"--hr-seek",
		"--idle", "--force-window", "--keep-open",
		"--no-resume-playback", "--no-save-position-on-quit",
		"--pause",
		"--quiet",
	)...)
	TrapDefer(func() {
		c.mpv.Process.Kill()
	})
	go func() {
		c.mpv.Wait()
		TrapExit(0)
	}()

	// NOTE: mpv sends these even if no file is loaded
	c.debouncer.debounceNext(messageTypePause, nil)
	c.debouncer.debounceNext(messageTypeSpeed, float64(1))

	restartChan, _, err := c.ipc.Subscribe("playback-restart")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	seekChan, _, err := c.ipc.Subscribe("seek")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	pauseChan, _, err := c.ipc.ObserveProperty("pause")
	if err != nil { Error("Failed to observe property:", err) }

	speedChan, _, err := c.ipc.ObserveProperty("speed")
	if err != nil { Error("Failed to observe property:", err) }

	// SECTION: graceful shutdown
	TrapDefer(func() {
		c.shuttingDown.Store(true)
	})

	// SECTION: heartbeat
	heartbeatTicker := time.NewTicker(heartbeatInterval)

	// SECTION: message loop
	var breakErr error
	outer: for {
		select {
		case msg := <- c.receiveChan:
			if err, ok := msg.payload.(error); ok {
				breakErr = err
				break outer
			}

			switch msg.typ {
			case messageTypePlay:
				m := msg.payload.(messagePlay)

				c.waitTime  = m.time
				c.waiting   = true
				c.sendReady = true

				c.debouncer.debounceNext(messageTypeSpeed, m.speed)
				c.debouncer.debounceNext(messageTypeSeek, nil)

				url := m.url
				if url == "" {
					url = "http://127.0.0.1:" + proxyPort
				}

				var options string
				if m.title != "" {
					options = fmt.Sprintf("force-media-title=%s,speed=%f,start=%f", m.title, m.speed, m.time)
				} else {
					options = fmt.Sprintf("speed=%f,start=%f", m.speed, m.time)
				}

				c.ipc.Request("loadfile", url, "replace", -1, options)

			case messageTypePause:
				c.debouncer.debounceNext(messageTypePause, nil)
				c.ipc.Request("set_property", "pause", true)
			case messageTypeResume:
				c.debouncer.debounceNext(messageTypeResume, nil)
				c.ipc.Request("set_property", "pause", false)
			case messageTypeSeek:
				c.sendReady = true

				c.debouncer.debounceNext(messageTypePause, nil)
				c.ipc.Request("set_property", "pause", true)

				time := msg.payload.(float64)
				c.debouncer.debounceNext(messageTypeSeek, time)
				c.ipc.Request("set_property", "playback-time", time)
			case messageTypeSpeed:
				speed := msg.payload.(float64)
				c.debouncer.debounceNext(messageTypeSpeed, speed)
				c.ipc.Request("set_property", "speed", speed)

			case messageTypeWaitStart:
				c.waitTime  = msg.payload.(float64)
				c.waiting   = true
				c.sendReady = true

				c.debouncer.debounceNext(messageTypePause, nil)
				c.ipc.Request("set_property", "pause", true)

				c.debouncer.debounceNext(messageTypeSeek, c.waitTime)
				c.ipc.Request("set_property", "playback-time", c.waitTime)
			case messageTypeWaitDone:
				c.waiting = false
				if msg.payload.(bool) {
					c.debouncer.debounceNext(messageTypeResume, nil)
					c.ipc.Request("set_property", "pause", false)
				}
			}

		case <-heartbeatTicker.C:
			err := sendMessage(c.messageConn, messageTypeHeartbeat, nil)
			if err != nil {
				breakErr = err
				break outer
			}

		case <-restartChan:
			if c.sendReady {
				c.sendReady = false
				err := sendMessage(c.messageConn, messageTypeReady, nil)
				if err != nil {
					breakErr = err
					break outer
				}
			}

		case <-seekChan:
			if c.debouncer.debounceOrUpdate(messageTypeSeek, nil) {
				break
			}

			if c.waiting {
				c.debouncer.debounceNext(messageTypeSeek, nil)
				c.ipc.Request("set_property", "playback-time", c.waitTime)
				break
			}

			c.sendReady = true

			timeAny := <-c.ipc.Request("get_property", "playback-time")
			if err, ok := timeAny.(error); ok {
				Error("Failed to get playback time:", err)
				break
			}
			time := timeAny.(float64)

			err := sendMessage(c.messageConn, messageTypeSeek, time)
			if err != nil {
				breakErr = err
				break outer
			}

		case pauseAny := <-pauseChan:
			pause := pauseAny.(bool)

			var typ messageType
			if pause {
				typ = messageTypePause
			} else {
				typ = messageTypeResume
			}

			if c.debouncer.debounceOrUpdate(typ, nil) {
				break
			}

			if c.waiting {
				if !pause {
					c.debouncer.debounceNext(messageTypePause, nil)
					c.ipc.Request("set_property", "pause", true)

					c.debouncer.debounceNext(messageTypeSeek, nil)
					c.ipc.Request("set_property", "playback-time", c.waitTime)
				}

				break
			}

			err := sendMessage(c.messageConn, typ, nil)
			if err != nil {
				breakErr = err
				break outer
			}

		case speedAny := <-speedChan:
			speed := speedAny.(float64)

			if c.debouncer.debounceOrUpdate(messageTypeSpeed, speed) {
				break
			}

			err := sendMessage(c.messageConn, messageTypeSpeed, speed)
			if err != nil {
				breakErr = err
				break outer
			}
		}
	}

	if c.shuttingDown.Load() || filterConnectionError(breakErr) == nil {
		Note("Disconnected")
	} else {
		Error("Disconnected:", unwrapOpError(breakErr))
	}
}

func isValidAddress(address string) bool {
	_, err := tailcat.ParseAddr(tailcat.Addr(address))
	return err == nil
}
