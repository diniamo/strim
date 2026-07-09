package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"sync"

	gostream "github.com/libp2p/go-libp2p-gostream"
	"github.com/libp2p/go-libp2p/core/discovery"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"

	"github.com/diniamo/gopv"
)

type client struct {
	// The message connection.
	messageConn net.Conn
	// The channel used for incoming messages.
	receiveChan chan message
	// The channel used for outgoing messages.
	sendChan chan message

	// The mpv process.
	mpv *exec.Cmd
	// The IPC client.
	ipc *gopv.Client
	// The debouncer used for ignoring events that were caused by our own requests.
	debouncer debouncer
	// The port of the local HTTP proxy for serving local (to the server) media.
	port string

	// Whether the session is currently waiting.
	// All media controls should be blocked, except speed.
	waiting bool
	// The time at which the wait started.
	// Used for preventing unpauses by seeking back to here.
	waitTime float64
	// Whether a ready message should be sent on the next playback-restart event.
	sendReady bool
}

func runClient(args []string) {
	// NOTE: input
	var phrase  string
	var mpvArgs []string
	if len(args) > 0 {
		phrase  = args[0]
		mpvArgs = args[1:]
	} else {
		phrase = readLine("Phrase: ")
	}

	// NOTE: trap setup
	TrapRegister()
	defer TrapRun()

	// NOTE: client data
	c := &client{}

	// NOTE: connection
	h, idht := createHost()
	TrapDefer(func() {
		h.Close()
	})

	disc          := routing.NewRoutingDiscovery(idht)
	peerChan, err := disc.FindPeers(context.Background(), phrase, discovery.Limit(1))
	if err != nil { Fatal("Failed to start discovery:", err) }

	// NOTE: message connection
	serverInfo := <-peerChan
	c.messageConn, err = gostream.Dial(context.Background(), h, serverInfo.ID, messageProtocol)
	if err != nil { Fatal("Failed to connect to server:", err) }

	Success("Connected")

	c.receiveChan = make(chan message)
	var done sync.Mutex
	go func() {
		var data [4096]byte

		for {
			// NOTE: done is unlocked by the consumer
			// this avoids overwriting the data that might still be used
			done.Lock()

			typ, payload, err := waitMessage(c.messageConn, data[:])
			if err != nil {
				close(c.receiveChan)
				break
			}

			c.receiveChan <- message{typ, payload}
		}
	}()

	c.sendChan = make(chan message)

	// NOTE: mpv
	setupPath()

	c.mpv, c.ipc = mpv(append(mpvArgs,
		// IMPORTANT: imprecise seeks may result in multiple seconds of desync,
		// since what would be the precise seek is reported anyway
		"--hr-seek",
		"--force-window", "--idle",
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

	c.debouncer = make(debouncer, debounceTypes)

	// NOTE: mpv sends these even if no file is loaded
	c.debouncer.push(messageTypePause)
	c.debouncer.push(messageTypeSpeed)

	restartChan, _, err := c.ipc.Subscribe("playback-restart")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	seekChan, _, err := c.ipc.Subscribe("seek")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	pauseChan, _, err := c.ipc.ObserveProperty("pause")
	if err != nil { Error("Failed to observe property:", err) }

	speedChan, _, err := c.ipc.ObserveProperty("speed")
	if err != nil { Error("Failed to observe property:", err) }

	// NOTE: message loop
	outer: for {
		select {
		case msg, ok := <- c.receiveChan:
			if !ok {
				break outer
			}

			switch msg.typ {
			case messageTypePlay:
				// IMPORTANT: we must not block the outer loop, becuase that would result in a deadlock, since we don't drain the channels
				go func() {
					m := msg.payload.(messagePlay)

					event, unsubscribe, err := c.ipc.Subscribe("file-loaded")
					if err != nil { Fatal("Failed to subscribe to event:", err) }

					path := m.url
					if path == "" {
						if c.port == "" {
							c.port = startProxy(h, serverInfo.ID)
						}

						path = "http://localhost:" + c.port
					}
					c.ipc.Request("loadfile", path)
					<-event
					unsubscribe()

					if m.title != "" {
						c.ipc.Request("set_property", "title", m.title)
					}

					c.debouncer.push(messageTypeSpeed)
					c.ipc.Request("set_property", "speed", m.speed)

					c.debouncer.push(messageTypeSeek)
					c.ipc.Request("set_property", "playback-time", m.time)

					c.waitTime  = m.time
					c.waiting   = true
					c.sendReady = true
				}()

			case messageTypePause:
				c.debouncer.push(messageTypePause)
				c.ipc.Request("set_property", "pause", true)
			case messageTypeResume:
				c.debouncer.push(messageTypeResume)
				c.ipc.Request("set_property", "pause", false)
			case messageTypeSeek:
				c.sendReady = true
				c.debouncer.push(messageTypeSeek)
				c.ipc.Request("set_property", "playback-time", msg.payload.(float64))
			case messageTypeSpeed:
				c.debouncer.push(messageTypeSpeed)
				c.ipc.Request("set_property", "speed", msg.payload.(float64))

			case messageTypeWaitStart:
				c.waiting  = true
				c.waitTime = msg.payload.(float64)
			case messageTypeWaitDone:
				c.waiting = false
				if msg.payload.(bool) {
					c.debouncer.push(messageTypeResume)
					c.ipc.Request("set_property", "pause", false)
				}
			}

			done.Unlock()

		case msg := <-c.sendChan:
			sendMessage(c.messageConn, msg.typ, msg.payload)

		case <-restartChan:
			if c.sendReady {
				c.sendReady = false
				sendMessage(c.messageConn, messageTypeReady, nil)
			}

		case <-seekChan:
			if c.debouncer.pop(messageTypeSeek) {
				break
			}

			if c.waiting {
				c.debouncer.push(messageTypeSeek)
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

			sendMessage(c.messageConn, messageTypeSeek, time)

		case pauseAny := <-pauseChan:
			pause := pauseAny.(bool)

			var typ messageType
			if pause {
				typ = messageTypePause
			} else {
				typ = messageTypeResume
			}

			if c.debouncer.pop(typ) {
				break
			}

			if c.waiting {
				if !pause {
					c.debouncer.push(messageTypePause)
					c.ipc.Request("set_property", "pause", true)

					c.debouncer.push(messageTypeSeek)
					c.ipc.Request("set_property", "playback-time", c.waitTime)
				}

				break
			}

			sendMessage(c.messageConn, typ, nil)

		case speedAny := <-speedChan:
			if c.debouncer.pop(messageTypeSpeed) {
				break
			}

			speed := speedAny.(float64)
			sendMessage(c.messageConn, messageTypeSpeed, speed)
		}
	}

	Note("Disconnected")
}

// NOTE: http proxy
type httpProxy struct {
	host       host.Host
	serverID   peer.ID
}
func (p *httpProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := gostream.Dial(context.Background(), p.host, p.serverID, streamProtocol)
	if err != nil { Fatal("Failed to connect to server:", err) }
	defer conn.Close()

	err = r.Write(conn)
	if err != nil {
		Error("HTTP request failed:", err)
		return
	}

	response, err := http.ReadResponse(bufio.NewReader(conn), r)
	if err != nil {
		Error("Failed to read HTTP response:", err)
		return
	}
	defer response.Body.Close()

	header := w.Header()
	for k, v := range response.Header {
		header[k] = v
	}

	w.WriteHeader(response.StatusCode)
	io.Copy(w, response.Body)
}

func startProxy(h host.Host, id peer.ID) string {
	listener, err := net.Listen("tcp", ":0")
	if err != nil { Fatal("Failed to start proxy listener:", err) }

	go func() {
		proxy := httpProxy{h, id}
		err   := http.Serve(listener, &proxy)
		if err != nil { Fatal("Failed to serve proxy:", err) }
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	return strconv.Itoa(port)
}
