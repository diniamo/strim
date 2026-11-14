package main

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"sync"

	gostream "github.com/libp2p/go-libp2p-gostream"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"

	"github.com/LordOfTrident/ansi-go"
	"github.com/diniamo/gopv"
	"golang.design/x/clipboard"
)

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
}

// The representation of a client on the server-side.
type serverClient struct {
	// The message stream of the client.
	stream network.Stream
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
	// The message stream of the incoming connection.
	stream network.Stream
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
}

func runServer(args []string) {
	// NOTE: input
	if len(args) == 0 {
		path := readLine("Path/URL: ")
		args = []string{path}
	}

	// NOTE: trap setup
	TrapRegister()
	defer TrapRun()

	// NOTE: clipboard setup
	err := clipboard.Init()
	if err != nil { Fatal("Failed to initialize clipboard:", err) }

	// NOTE: server data
	s := server{
		debouncer: newDebouncer(),
		cmdChan:   make(chan any),
		clients:   make(map[int]*serverClient),
	}

	// NOTE: P2P setup
	h := createHost()
	TrapDefer(func() {
		h.Close()
	})

	subscription, err := h.EventBus().Subscribe([]any{
		new(event.EvtLocalReachabilityChanged),
		new(event.EvtLocalAddressesUpdated),
	})
	if err != nil { Fatal("Failed to subscribe to host events:", err) }

	encodedIDChan := make(chan string)
	go func() {
		eventChan    := subscription.Out()
		reachability := network.ReachabilityUnknown
		for {
			e := <-eventChan
			if e, ok := e.(event.EvtLocalReachabilityChanged); ok {
				reachability = e.Reachability
			}

			if reachability != network.ReachabilityUnknown {
				var filtered []ma.Multiaddr
				for _, addr := range h.Addrs() {
					if manet.IsPublicAddr(addr) && hasGoodTransport(addr) {
						filtered = append(filtered, addr)
					}
				}

				encoded := encodeInfo(peer.AddrInfo{ID: h.ID(), Addrs: filtered})
				encodedIDChan <- encoded
			}
		}
	}()

	var previousID string

	// NOTE: message handler
	h.SetStreamHandler(messageProtocol, func(stream network.Stream) {
		s.cmdChan <- cmdClientConnected{stream}
	})

	// NOTE: mpv
	setupPath()

	// IMPORTANT: imprecise seeks may result in multiple seconds of desync,
	// since what would be the precise seek is reported anyway
	s.mpv, s.ipc = mpv(append(args, "--hr-seek", "--quiet")...)
	TrapDefer(func() {
		s.mpv.Process.Kill()
	})
	go func() {
		s.mpv.Wait()
		TrapExit(0)
	}()

	loadedChan, _, err := s.ipc.Subscribe("file-loaded")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	restartChan, _, err := s.ipc.Subscribe("playback-restart")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	seekChan, _, err := s.ipc.Subscribe("seek")
	if err != nil { Fatal("Failed to subscribe to event:", err) }

	pauseChan, _, err := s.ipc.ObserveProperty("pause")
	if err != nil { Error("Failed to observe property:", err) }

	speedChan, _, err := s.ipc.ObserveProperty("speed")
	if err != nil { Error("Failed to observe property:", err) }

	for {
		select {
		case encodedID := <-encodedIDChan:
			if encodedID != previousID {
				clipboardChan := clipboard.Write(clipboard.FmtText, []byte(encodedID))

				var clipboardText string
				if clipboardChan != nil {
					clipboardText = " (copied to clipboard)"
				}
				ansi.Eprintln(ansi.Bold, "New ID", ansi.Reset, clipboardText, ": ", ansi.Dim, encodedID)

				previousID = encodedID
			}
		case cmd := <-s.cmdChan:
			switch cmd := cmd.(type) {
			case cmdClientConnected:
				s.waitCount += 1
				s.waitStart()

				var time float64
				timeAny := <-s.ipc.Request("get_property", "playback-time")
				if err, ok := timeAny.(error); ok {
					Error("Failed to get playback time:", err)
				} else {
					time = timeAny.(float64)
				}

				sendMessage(cmd.stream, messageTypePlay, messagePlay{s.url, s.title, time, s.speed})

				id := s.clientID
				s.clientID += 1

				client := &serverClient{
					stream: cmd.stream,
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
						s.debouncer.push(messageTypeSpeed)
						s.ipc.Request("set_property", "speed", cmd.payload.(float64))

						for id, c := range s.clients {
							if id != cmd.id {
								sendMessage(c.stream, cmd.typ, cmd.payload)
							}
						}
					}
				} else {
					switch cmd.typ {
					case messageTypePause:
						s.debouncer.push(messageTypePause)
						s.ipc.Request("set_property", "pause", true)
					case messageTypeResume:
						s.debouncer.push(messageTypeResume)
						s.ipc.Request("set_property", "pause", false)
					case messageTypeSeek:
						s.waitAll()
						s.debouncer.push(messageTypeSeek)
						s.ipc.Request("set_property", "playback-time", cmd.payload.(float64))
					case messageTypeSpeed:
						s.debouncer.push(messageTypeSpeed)
						s.ipc.Request("set_property", "speed", cmd.payload.(float64))
					}

					for id, c := range s.clients {
						if id != cmd.id {
							sendMessage(c.stream, cmd.typ, cmd.payload)
						}
					}
				}

			case cmdClientDisconnected:
				if !cmd.client.ready {
					s.waitCount -= 1
					s.waitCheck()
				}

				delete(s.clients, cmd.id)
				Notef("Client %d disconnected", cmd.id)
			}

		case <-loadedChan:
			if len(s.clients) > 0 {
				s.waitAll()
			}

			rawPathAny := <-s.ipc.Request("get_property", "path")
			if err, ok := rawPathAny.(error); ok { Fatal("Failed to get path:", err) }
			rawPath := rawPathAny.(string)

			var protocol, path string
			components := strings.Split(rawPath, "://")
			if len(components) > 1 {
				protocol = components[0]
				path     = components[1]
			} else {
				protocol = "file"
				path     = components[0]
			}

			switch protocol {
			case "file":
				if s.fileServer != nil {
					s.fileServer.mu.Lock()
					s.fileServer.path = path
					s.fileServer.mu.Unlock()
				} else {
					s.fileServer = &fileServer{path: path}

					go func() {
						listener, err := gostream.Listen(h, streamProtocol)
						if err != nil { Fatal("Failed to listen for stream connections:", err) }

						server := http.Server{Handler: s.fileServer}
						err = server.Serve(listener)
						if err != nil && !errors.Is(err, http.ErrServerClosed) { Fatal("File server failed:", err) }
					}()
				}

				titleAny := <-s.ipc.Request("get_property", "media-title")
				if err, ok := titleAny.(error); ok {
					Error("Failed to get title:", err)
				} else {
					s.title = titleAny.(string)
				}
				s.url = ""

			case "http", "https":
				if s.fileServer != nil {
					s.fileServer.mu.Lock()
					s.fileServer.path = ""
					s.fileServer.mu.Unlock()
				}

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
				for _, c := range s.clients {
					sendMessage(c.stream, messageTypePlay, msg)
				}
			}

		case <-restartChan:
			s.ready = true
			s.waitCheck()

		case <-seekChan:
			if s.debouncer.pop(messageTypeSeek) {
				break
			}

			if s.waiting {
				s.debouncer.push(messageTypeSeek)
				s.ipc.Request("set_property", "playback-time", s.waitTime)
				break
			}

			if len(s.clients) <= 0 {
				break
			}

			s.waitAll()

			timeAny := <-s.ipc.Request("get_property", "playback-time")
			if err, ok := timeAny.(error); ok {
				Error("Failed to get playback time:", err)
				break
			}
			time := timeAny.(float64)

			for _, c := range s.clients {
				sendMessage(c.stream, messageTypeSeek, time)
			}

		case pauseAny := <-pauseChan:
			pause := pauseAny.(bool)

			var typ messageType
			if pause {
				typ = messageTypePause
			} else {
				typ = messageTypeResume
			}

			if s.debouncer.pop(typ) {
				break
			}

			if s.waiting {
				if !pause {
					s.pause = true
					s.debouncer.push(messageTypePause)
					s.ipc.Request("set_property", "pause", true)

					s.debouncer.push(messageTypeSeek)
					s.ipc.Request("set_property", "playback-time", s.waitTime)
				}

				break
			}

			s.pause = pause

			for _, c := range s.clients {
				sendMessage(c.stream, typ, nil)
			}

		case speedAny := <-speedChan:
			if s.debouncer.pop(messageTypeSpeed) {
				break
			}

			s.speed = speedAny.(float64)

			for _, c := range s.clients {
				sendMessage(c.stream, messageTypeSpeed, s.speed)
			}
		}
	}
}

func hasGoodTransport(addr ma.Multiaddr) bool {
	for _, proto := range addr.Protocols() {
		if proto.Code == ma.P_QUIC_V1 || proto.Code == ma.P_TCP {
			return true
		}
	}

	return false
}

func encodeInfo(info peer.AddrInfo) string {
	var buf []byte

	// ID
	id := []byte(info.ID)

	buf = binary.AppendUvarint(buf, uint64(len(id)))
	buf = append(buf, id...)

	// Addresses
	buf = binary.AppendUvarint(buf, uint64(len(info.Addrs)))
	for _, addr := range info.Addrs {
		bytes := addr.Bytes()
		buf = binary.AppendUvarint(buf, uint64(len(bytes)))
		buf = append(buf, bytes...)
	}

	return base64.RawURLEncoding.EncodeToString(buf)
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
	var data [4096]byte
	for {
		typ, payload, err := waitMessage(c.stream, data[:])
		if err != nil { break }

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

	c.stream.Close()

	s.cmdChan <- cmdClientDisconnected{id, c}
}

func (s *server) waitStart() {
	if !s.waiting {
		s.waiting    = true
		s.waitResume = !s.pause

		if s.waitResume {
			s.pause = true
			s.debouncer.push(messageTypePause)
			s.ipc.Request("set_property", "pause", true)
		}

		timeAny := <-s.ipc.Request("get_property", "playback-time")
		if err, ok := timeAny.(error); ok {
			Error("Failed to get playback time:", err)
		} else {
			s.waitTime = timeAny.(float64)
		}

		for _, c := range s.clients {
			sendMessage(c.stream, messageTypeWaitStart, s.waitTime)
		}
	}
}

func (s *server) waitAll() {
	s.ready = false
	for _, c := range s.clients {
		c.ready = false
	}

	s.waitCount = len(s.clients)
	s.waitStart()
}

func (s *server) waitCheck() {
	if s.waiting && s.ready && s.waitCount <= 0 {
		s.waiting = false

		if s.waitResume {
			s.pause = false
			s.debouncer.push(messageTypeResume)
			s.ipc.Request("set_property", "pause", false)
		}

		for _, c := range s.clients {
			sendMessage(c.stream, messageTypeWaitDone, s.waitResume)
		}
	}
}
