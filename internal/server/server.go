package server

import (
	"context"
	"net"
	"net/http"

	log "github.com/diniamo/glog"
	"github.com/diniamo/gopv"
	"github.com/diniamo/strim/internal/mpv"
	"github.com/diniamo/strim/internal/proto"
)

const Port = "5300"

const serverID = -1
const invalidID = -2

type Server struct {
	title string
	
	cmux *cMux
	fs http.Server
	// These represent the INITIAL initilization of the file server
	fsInit bool
	fsInitChan chan struct{}

	ipc *gopv.Client
	debouncer mpv.Debouncer
	pause bool

	clients []*Client
	initCount int
	resumeWhenReady bool
	aliveCount int
	buf [1024]byte
}

func New(ipc *gopv.Client) *Server {
	return &Server{
		fsInitChan: make(chan struct{}),

		ipc: ipc,
		debouncer: make(mpv.Debouncer, 3),
		
		clients: []*Client{},
	}
}

func (s *Server) RegisterHandlers() {
	s.ipc.RegisterListener("file-loaded", func(_ map[string]any) {
		reinit := s.fsInit && s.aliveCount > 0
		if reinit {
			log.Note("Reinitializing")

			_, err := s.ipc.Request("set_property", "pause", true)
			if err != nil {
				log.Errorf("Failed to pause: %s", err)
			}
			
			s.dispatch(serverID, &proto.Packet{Type: proto.PacketTypeIdle})
		}

		title, err := s.ipc.Request("get_property", "media-title")
		if err == nil {
			s.title = title.(string)
		} else {
			log.Errorf("Failed to get title: %s", err)
		}

		path, err := s.ipc.Request("get_property", "path")
		if err != nil {
			log.Fatalf("Failed to get path: %s", err)
		}

		s.fs.Shutdown(context.Background())
		s.fs = http.Server{Handler: &fileServer{title.(string), path.(string)}}
		// Shutdown closes the associated listener
		s.cmux.stream = newCMuxListener()
		go s.serveStream()

		if reinit {
			time, err := s.ipc.Request("get_property", "playback-time")
			if err != nil {
				log.Errorf("Failed to get playback time: %s", err)
			}

			s.initCount = s.aliveCount
			s.resumeWhenReady = s.aliveCount > 0
		
			s.dispatch(serverID, &proto.Packet{
				Type: proto.PacketTypeInit,
				Payload: proto.EncodeInit(s.title, time.(float64)),
			})
		} else if !s.fsInit {
			s.fsInit = true
			s.fsInitChan <- struct{}{}
			close(s.fsInitChan)
		}
	})

	_, err := s.ipc.ObserveProperty("pause", func(state any) {
		s.pause = state.(bool)

		packetType := proto.PacketTypeResume
		if s.pause {
			packetType = proto.PacketTypePause
		}
		
		if !s.debouncer.IsDebounce(packetType) {
			s.dispatch(serverID, &proto.Packet{Type: packetType})
		}
	})
	if err != nil {
		log.Errorf("Failed to register pause handler: %s", err)
	}

	s.ipc.RegisterListener("seek", func(_ map[string]any) {
		if s.debouncer.IsDebounce(proto.PacketTypeSeek) {
			return
		}

		time, err := s.ipc.Request("get_property", "playback-time")
		if err != nil {
			log.Errorf("Failed to get playback time: %s", err)
			return
		}

		s.dispatch(serverID, &proto.Packet{
			Type: proto.PacketTypeSeek,
			Payload: proto.EncodeSeek(time.(float64)),
		})
	})
}

func (s *Server) ListenAndServe() {
	listener, err := net.Listen("tcp", ":" + Port)
	if err != nil {
		log.Fatalf("Failed to listen: %s", err)
	}
	
	s.cmux = &cMux{
		listener: listener,
		message: newCMuxListener(),
	}
	defer s.cmux.Close()

	// The initial FS stream is started in the file-loaded event,
	// and the message server relies on the title variable which is set there too.
	<-s.fsInitChan
	go s.serveMessage()

	log.Successf("Listening on port %s", Port)
	
	err = s.cmux.Serve()
	if err != nil {
		log.Fatalf("Failed to serve multiplexer: %s", err)
	}
}

func (s *Server) serveMessage() {
	for {
		conn, err := s.cmux.message.Accept()
		if err != nil {
			log.Warnf("Failed to accept connection: %s", err)
			continue
		}
		
		client := &Client{
			id: len(s.clients),
			alive: true,
			conn: proto.NewConn(conn),
		}
		
		log.Notef("Client %d: connected", client.id)

		s.resumeWhenReady = s.resumeWhenReady || !s.pause
		if !s.pause {
			s.dispatch(invalidID, &proto.Packet{Type: proto.PacketTypePause})
		}

		time, err := s.ipc.Request("get_property", "playback-time")
		if err != nil {
			log.Errorf("Client %d: failed to get playback time: %s, disconnecting", client.id, err)
			client.Close()
			continue
		}

		err = client.conn.WritePacket(&proto.Packet{
			Type: proto.PacketTypeInit,
			Payload: proto.EncodeInit(s.title, time.(float64)),
		})
		if err != nil {
			log.Errorf("Client %d: init packet failed: %s, disconnecting", client.id, err)
			client.Close()
			continue
		}
		
		s.clients = append(s.clients, client)
		s.aliveCount += 1
		s.initCount += 1
		
		go client.packetLoop(s)
	}	
}

// Never dispatches to the server
func (s *Server) dispatchRaw(except int, packet []byte) {
	for _, c := range s.clients {
		if c.id == except || !c.alive {
			continue
		}

		err := c.conn.WriteRaw(packet)
		if err != nil {
			log.Errorf("Client %d: failed to send packet: %s", c.id, err)
		}
	}
}

// Dispatches to the server, unless `except` is `serverID`
func (s *Server) dispatch(except int, packet *proto.Packet) {
	if except != serverID {
		err := mpv.PacketToIPC(packet, s.debouncer, s.ipc)
		if err != nil {
			log.Errorf("IPC request failed: %s", err)
		}
	}

	raw := proto.EncodePacket(packet, s.buf[:])
	s.dispatchRaw(except, raw)
}
