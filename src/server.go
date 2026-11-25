package main

import (
	"context"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/logger"

	"github.com/LordOfTrident/ansi-go"
	"github.com/diniamo/gopv"
	"golang.design/x/clipboard"
)

const invalidID int = -1

type server struct {
	// The mpv process.
	mpv *exec.Cmd
	// The IPC client.
	ipc *gopv.Client
	// The debouncer used for ignoring events that were caused by our own requests.
	debouncer debouncer

	// The clients that are currently connected.
	// A map is used to allow for easy removal.
	clients map[int]*serverClient
	// The ID the next client is going be assigned, which will be used to key clients.
	clientID int

	// The channel used to funnel server actions into the main thread.
	// This greatly simplifies concurrency by grouping all mutation into the same thread.
	// See the cmd* types below.
	cmdChan chan any
	// Whether the server is ready, which is required for the wait state to end
	ready bool
	// Whether the session is currently waiting.
	// All media controls should be blocked, except speed.
	waiting bool
	// The time at which the wait started.
	// Used for preventing unpauses by seeking back to here.
	waitTime float64
	// The number of clients still waiting.
	waitCount int
	// Whether playback should be resumed after the wait.
	waitResume bool

	// The fileserver.
	fileServer *fileServer

	// The title of the playing media.
	// Should be empty when the media is remote.
	title string
	// The URL of the playing media.
	// Should be empty when the media is local.
	url string
	// Whether playback is paused.
	// If we send a request to pause or resume, this should be updated next to that request,
	// because we may rely on this state before the pause event gets processed, and in this case
	// the next appropriate event has to be debounced as well.
	pause bool
	// The speed of the playback.
	// The same applies here (see above).
	speed float64

	// Whether the user requested that the media player or strim exit.
	// Errors are ignored if this is set.
	shuttingDown atomic.Bool
}

// The representation of a client on the server-side.
type serverClient struct {
	// The message conn of the client.
	conn net.Conn
	// Whether the client is ready.
	ready bool
}

// Fileserver for serving local media.
type fileServer struct {
	// The mutex used to protect path.
	mu sync.RWMutex
	// The local path to serve.
	path string
}

// Command for new connections.
type cmdClientConnected struct {
	// The message conn of the incoming connection.
	conn net.Conn
}

// Command to mark when a client is ready.
type cmdClientReady struct {
	// The client which is ready.
	client *serverClient
}

// Command for actions taken by a client.
type cmdClientAction struct {
	// The client which took the action.
	id int
	// The message type of the action.
	typ messageType
	// The payload of the action.
	payload any
}

// Command for when a client disconnects.
type cmdClientDisconnected struct {
	// The ID if the disconnected client.
	id int
	// The client which disconnected.
	client *serverClient
	// The error that caused it.
	err error
}

func runServer(args []string) {
	// SECTION: trap setup
	TrapRegister()
	defer TrapRun()

	// SECTION: clipboard setup
	err := clipboard.Init()
	if err != nil { Warn("Failed to initialize clipboard:", err) }

	// SECTION: input
	if len(args) == 0 {
		path := readLine("Path/URL: ")
		args = []string{path}
	}

	// SECTION: server data
	fs := fileServer{}
	s := server{
		cmdChan:    make(chan any),
		clients:    make(map[int]*serverClient),
		fileServer: &fs,
	}

	// SECTION: Tailcat setup
	ts := &tailcat.Server{Logf: logger.Discard}
	TrapDefer(func() {
		for _, c := range s.clients {
			c.conn.Close()
		}

		ts.Close()
	})

	// NOTE: the protocol specified here only refers to the in-tunnel one,
	// the actual protocol is promoted to UDP by magicsock if possible.
	messageListener, err := ts.Listen(context.Background(), "tcp", messageAddress)
	if err != nil { Fatal("Failed to listen for connections:", err) }

	streamListener, err := ts.Listen(context.Background(), "tcp", streamAddress)
	if err != nil { Fatal("Failed to listen for connections:", err) }

	go func() {
		for {
			conn, err := messageListener.Accept()
			if err != nil {
				if !s.shuttingDown.Load() { Fatal("Failed to accept connection:", err) }
				break
			}

			s.cmdChan <- cmdClientConnected{conn}
		}
	}()

	go func() {
		hs := http.Server{Handler: s.fileServer}
		err := hs.Serve(streamListener)
		if !s.shuttingDown.Load() && err != nil { Fatal("File server failed:", err) }
	}()

	address := string(ts.TailcatAddr())
	clipboardChan := clipboard.Write(clipboard.FmtText, []byte(address))
	clipboardText := ""
	if clipboardChan != nil {
		clipboardText = " (copied to clipboard)"
	}
	ansi.Eprintln(ansi.Bold, "Address", ansi.Reset, clipboardText, ": ", ansi.Dim, address)

	// SECTION: mpv
	setupPath()

	// IMPORTANT: imprecise seeks may result in multiple seconds of desync,
	// since what would be the precise seek is reported anyway
	s.mpv, s.ipc = mpv(append(args, "--hr-seek", "--keep-open", "--msg-level=all=error")...)
	TrapDefer(func() {
		s.mpv.Process.Kill()
	})
	go func() {
		s.mpv.Wait()
		TrapExit(0)
	}()

	// IMPORTANT: subscribe to these at the same time, so we get an ordering guarantee.
	// Order may be random with separate subscriptions, because if multiple are pending
	// when the select statement is reached, one is selected at random.
	eventChan, _, err := s.ipc.Subscribe("file-loaded", "playback-restart", "seek")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	pauseChan, _, err := s.ipc.ObserveProperty("pause")
	if err != nil { Error("Failed to observe property:", err) }

	speedChan, _, err := s.ipc.ObserveProperty("speed")
	if err != nil { Error("Failed to observe property:", err) }

	// SECTION: graceful shutdown
	exitChan := make(chan struct{})
	TrapDefer(func() {
		s.shuttingDown.Store(true)
		close(exitChan)
	})

	// SECTION: main loop
	outer: for {
		select {
		case cmd := <-s.cmdChan:
			switch cmd := cmd.(type) {
			case cmdClientConnected:
				s.waitCount += 1
				s.waitStart(nil)

				var time float64
				timeAny := <-s.ipc.Request("get_property", "playback-time")
				if err, ok := timeAny.(error); ok {
					Error("Failed to get playback time:", err)
				} else {
					time = timeAny.(float64)
				}

				err := sendMessage(cmd.conn, messageTypePlay, messagePlay{s.url, s.title, time, s.speed})
				if err != nil {
					Warn("Failed to send initial message to new client:", err)
					cmd.conn.Close()
					s.waitCount -= 1
					s.waitCheck()
					break
				}

				id := s.clientID
				s.clientID += 1

				client := &serverClient{
					conn: cmd.conn,
					ready:  false,
				}
				s.clients[id] = client

				go s.serve(id, client)
				Notef("Client %d connected", id)

			case cmdClientReady:
				if !cmd.client.ready {
					cmd.client.ready = true
					s.waitCount -= 1
					s.waitCheck()
				}

			case cmdClientAction:
				if s.waiting {
					if cmd.typ == messageTypeSpeed {
						s.speed = cmd.payload.(float64)
						s.debouncer.debounceNext(messageTypeSpeed, s.speed)
						s.ipc.Request("set_property", "speed", s.speed)

						s.sendAll(cmd.typ, cmd.payload, cmd.id)
					}
				} else {
					switch cmd.typ {
					case messageTypePause:
						s.pause = true
						s.debouncer.debounceNext(messageTypePause, nil)
						s.ipc.Request("set_property", "pause", true)
						s.sendAll(cmd.typ, cmd.payload, cmd.id)
					case messageTypeResume:
						s.pause = false
						s.debouncer.debounceNext(messageTypeResume, nil)
						s.ipc.Request("set_property", "pause", false)
						s.sendAll(cmd.typ, cmd.payload, cmd.id)
					case messageTypeSeek:
						time := cmd.payload.(float64)
						s.debouncer.debounceNext(messageTypeSeek, nil)
						s.ipc.Request("set_property", "playback-time", time)
						s.waitAll(&time)
					case messageTypeSpeed:
						s.speed = cmd.payload.(float64)
						s.debouncer.debounceNext(messageTypeSpeed, s.speed)
						s.ipc.Request("set_property", "speed", s.speed)
						s.sendAll(cmd.typ, cmd.payload, cmd.id)
					}
				}

			case cmdClientDisconnected:
				delete(s.clients, cmd.id)

				if !cmd.client.ready {
					s.waitCount -= 1
					s.waitCheck()
				}

				if !s.shuttingDown.Load() {
					switch {
					case filterConnectionError(cmd.err) == nil:
						Notef("Client %d disconnected", cmd.id)
					default:
						Warnf("Client %d disconnected: %s", cmd.id, unwrapOpError(cmd.err))
					}
				}
			}

		case event := <-eventChan:
			switch event.Name {
			case "file-loaded":
				if len(s.clients) > 0 {
					s.waitAll(nil)
				}

				rawPathAny := <-s.ipc.Request("get_property", "path")
				if err, ok := rawPathAny.(error); ok { Fatal("Failed to get path:", err) }
				rawPath := rawPathAny.(string)

				var protocol, path string
				components := strings.SplitN(rawPath, "://", 2)
				if len(components) > 1 {
					protocol = components[0]
					path     = components[1]
				} else {
					protocol = "file"
					path     = components[0]
				}

				switch protocol {
				case "file":
					s.fileServer.mu.Lock()
					s.fileServer.path = path
					s.fileServer.mu.Unlock()

					titleAny := <-s.ipc.Request("get_property", "media-title")
					if err, ok := titleAny.(error); ok {
						Error("Failed to get title:", err)
					} else {
						s.title = titleAny.(string)
					}
					s.url = ""

				case "http", "https":
					s.fileServer.mu.Lock()
					s.fileServer.path = ""
					s.fileServer.mu.Unlock()

					s.title = ""
					s.url   = rawPath

				default:
					Fatal("Unsupported protocol:", protocol)
				}

				if len(s.clients) > 0 {
					var time float64
					timeAny := <-s.ipc.Request("get_property", "playback-time")
					if err, ok := timeAny.(error); ok {
						Error("Failed to get playback time:", err)
					} else {
						time = timeAny.(float64)
					}

					msg := messagePlay{s.url, s.title, time, s.speed}
					s.sendAll(messageTypePlay, msg, invalidID)
				}

			case "playback-restart":
				s.ready = true
				if s.waiting {
					showOverlay(s.ipc, "Waiting for others...")
					s.waitCheck()
				}

			case "seek":
				if s.debouncer.debounceOrUpdate(messageTypeSeek, nil) {
					break
				}

				if s.waiting {
					s.debouncer.debounceNext(messageTypeSeek, nil)
					s.ipc.Request("set_property", "playback-time", s.waitTime)
					break
				}

				if len(s.clients) == 0 {
					break
				}

				s.waitAll(nil)
			}

		case pauseAny := <-pauseChan:
			pause := pauseAny.(bool)

			var typ messageType
			if pause {
				typ = messageTypePause
			} else {
				typ = messageTypeResume
			}

			if s.debouncer.debounceOrUpdate(typ, nil) {
				break
			}

			if s.waiting {
				if !pause {
					s.pause = true
					s.debouncer.debounceNext(messageTypePause, nil)
					s.ipc.Request("set_property", "pause", true)

					s.debouncer.debounceNext(messageTypeSeek, nil)
					s.ipc.Request("set_property", "playback-time", s.waitTime)
				}

				break
			}

			s.pause = pause

			s.sendAll(typ, nil, invalidID)

		case speedAny := <-speedChan:
			s.speed = speedAny.(float64)

			if s.debouncer.debounceOrUpdate(messageTypeSpeed, s.speed) {
				break
			}

			s.sendAll(messageTypeSpeed, s.speed, invalidID)

		case <-exitChan:
			break outer
		}
	}
}

func (s *fileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	path := s.path
	s.mu.RUnlock()

	if path != "" {
		http.ServeFile(w, r, path)
	} else {
		http.Error(w, "no local media is playing", http.StatusServiceUnavailable)
	}
}

func (s *server) serve(id int, c *serverClient) {
	var data [maxMessageSize]byte
	var breakErr error

	for {
		c.conn.SetReadDeadline(time.Now().Add(heartbeatTimeout))
		typ, payload, err := waitMessage(c.conn, data[:])
		if err != nil {
			breakErr = err
			break
		}

		forward := false
		switch typ {
		case messageTypeReady:
			s.cmdChan <- cmdClientReady{c}

		case messageTypePause:  forward = true
		case messageTypeResume: forward = true
		case messageTypeSeek:   forward = true
		case messageTypeSpeed:  forward = true
		}

		if forward {
			s.cmdChan <- cmdClientAction{id, typ, payload}
		}
	}

	c.conn.Close()
	s.cmdChan <- cmdClientDisconnected{id, c, breakErr}
}

func (s *server) waitStart(time *float64) {
	if !s.waiting {
		s.waiting    = true
		s.waitResume = !s.pause

		if s.waitResume {
			s.pause = true
			s.debouncer.debounceNext(messageTypePause, nil)
			s.ipc.Request("set_property", "pause", true)
		}

		if s.ready {
			showOverlay(s.ipc, "Waiting for others...")
		} else {
			showOverlay(s.ipc, "Buffering...")
		}

		if time != nil {
			s.waitTime = *time

			s.debouncer.debounceNext(messageTypeSeek, s.waitTime)
			s.ipc.Request("set_property", "playback-time", s.waitTime)
		} else {
			timeAny := <-s.ipc.Request("get_property", "playback-time")
			if err, ok := timeAny.(error); ok {
				Error("Failed to get playback time:", err)
			} else {
				s.waitTime = timeAny.(float64)
			}
		}

		s.sendAll(messageTypeWaitStart, s.waitTime, invalidID)
	}
}

func (s *server) waitAll(time *float64) {
	s.ready = false
	for _, c := range s.clients {
		c.ready = false
	}

	s.waitCount = len(s.clients)
	s.waitStart(time)
}

func (s *server) waitCheck() {
	if s.waiting && s.ready && s.waitCount == 0 {
		s.waiting = false

		if s.waitResume {
			s.pause = false
			s.debouncer.debounceNext(messageTypeResume, nil)
			s.ipc.Request("set_property", "pause", false)
		}
		hideOverlay(s.ipc)

		s.sendAll(messageTypeWaitDone, s.waitResume, invalidID)
	}
}

func (s *server) sendAll(typ messageType, payload any, except int) {
	var buf [maxMessageSize]byte
	data := marshalMessage(buf[:], typ, payload)

	for id, c := range s.clients {
		if id == except { continue }

		_, err := c.conn.Write(data)
		if err != nil {
			c.conn.Close()
		}
	}
}
