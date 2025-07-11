package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"

	"time"

	log "github.com/diniamo/glog"
	"github.com/diniamo/gopv"
	"github.com/diniamo/strim/internal/mpv"
	"github.com/diniamo/strim/internal/proto"
)

const PortMessage = "5300"
const PortStream = "5301"

const serverID = -1
const invalidID = -2

type Server struct {
	title string
	addressStream string
	fs http.Server

	ipc *gopv.Client
	debouncer mpv.Debouncer
	pause bool

	clients []*Client
	initCount int
	aliveCount int
	buf []byte
}

type FileServer struct {
	title string
	path string
}

func New(ipc *gopv.Client) *Server {
	return &Server{
		addressStream: ":" + PortStream,
		ipc: ipc,
		debouncer: make(mpv.Debouncer, 3),
		initCount: 0,
		aliveCount: 0,
		clients: []*Client{},
		buf: make([]byte, 1024),
	}
}

func (s *Server) listenFS() {
	err := s.fs.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("File server failed: %s", err)
	}
}

func (s *FileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	file, err := os.Open(s.path)
	if err != nil {
		log.Errorf("Failed to open file: %s", err)
		return
	}
	defer file.Close()

	http.ServeContent(w, r, s.title, time.Time{}, file)
}

func (s *Server) waitPath() (string, error) {
	pathChan := make(chan string)
	defer close(pathChan)
	
	observer, err := s.ipc.ObserveProperty("path", func(data any) {
		path, ok := data.(string)
		if ok {
			pathChan <- path
		}
	})
	if err != nil {
		return "", err
	}

	path := <-pathChan
	s.ipc.UnobserveProperty(observer)

	return path, nil
}

func (s *Server) Listen() {
	path, err := s.waitPath()
	if err != nil {
		log.Fatalf("Failed to get path: %s", err)
	}

	title, err := s.ipc.Request("get_property", "title")
	if err != nil {
		log.Warnf("Failed to get title: %s", err)
	}
	s.title = title.(string)
	
	s.fs = http.Server{Addr: s.addressStream, Handler: &FileServer{s.title, path}}
	go s.listenFS()

	listener, err := net.Listen("tcp", ":" + PortMessage)
	if err != nil {
		log.Fatalf("Listener failed: %s", err)
	}
	defer listener.Close()

	log.Successf("Listening on port %s/%s", PortMessage, PortStream)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Warnf("Failed to accept connection: %s", err)
			continue
		}
		
		client := &Client{
			id: len(s.clients),
			alive: true,
			conn: proto.NewConn(conn),
		}
		s.clients = append(s.clients, client)
		s.aliveCount += 1
		
		log.Successf("Client %d: connected", client.id)

		time, err := s.ipc.Request("get_property", "playback-time")
		if err != nil {
			log.Errorf("Client %d: failed to get playback time: %s, disconnecting", client.id, err)
			client.Close()
			s.aliveCount -= 1
			continue
		}
		
		if !s.pause {
			s.dispatch(invalidID, &proto.Packet{Type: proto.PacketTypePause})
		}

		s.initCount += 1
		err = client.conn.WritePacket(&proto.Packet{
			Type: proto.PacketTypeInit,
			Payload: proto.EncodeInit(s.title, time.(float64)),
		})
		if err != nil {
			log.Errorf("Client %d: init packet failed: %s, disconnecting", client.id, err)
			client.Close()
			s.initCount -= 1
			s.aliveCount -= 1
			s.dispatch(invalidID, &proto.Packet{Type: proto.PacketTypeResume})
			continue
		}
		
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

	raw := proto.EncodePacket(packet, s.buf)
	s.dispatchRaw(except, raw)
}

func (s *Server) RegisterHandlers() {
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

	s.ipc.RegisterListener("file-loaded", func(_ map[string]any) {
		_, err := s.ipc.Request("set_property", "pause", true)
		if err != nil {
			log.Errorf("Failed to pause: %s", err)
		}
		s.dispatch(serverID, &proto.Packet{Type: proto.PacketTypeIdle})

		title, err := s.ipc.Request("get_property", "title")
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
		s.fs = http.Server{Addr: s.addressStream, Handler: &FileServer{title.(string), path.(string)}}
		go s.listenFS()
		
		time, err := s.ipc.Request("get_property", "playback-time")
		if err != nil {
			log.Errorf("Failed to get playback time: %s", err)
		}

		s.initCount = s.aliveCount
		s.dispatch(serverID, &proto.Packet{
			Type: proto.PacketTypeInit,
			Payload: proto.EncodeInit(s.title, time.(float64)),
		})
	})
}
