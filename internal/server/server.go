package server

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	log "github.com/diniamo/glog"
	"github.com/diniamo/gopv"
	"github.com/diniamo/strim/internal/common"
	"github.com/diniamo/strim/internal/mpv"
	"github.com/diniamo/strim/internal/proto"
)

const PortMessage = "5300"
const PortStream = "5301"

const serverID = -1
const invalidID = -2

type Server struct {
	path string
	title string
	
	ipc *gopv.Client
	debouncer mpv.Debouncer
	pause bool

	initCount int
	clients []*Client
	buf []byte
}

func NewServer(path string, ipc *gopv.Client) (s *Server) {
	return &Server{
		path: path,
		title: filepath.Base(path),
		ipc: ipc,
		initCount: 0,
		clients: []*Client{},
		buf: make([]byte, 1024),
		debouncer: make(mpv.Debouncer, 3),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	file, err := os.Open(s.path)
	if err != nil {
		log.Errorf("Failed to open file: %s", err)
		return
	}
	defer file.Close()

	http.ServeContent(w, r, s.title, time.Time{}, file)
}

func (s *Server) Listen() {
	listener, err := net.Listen("tcp", ":" + PortMessage)
	if err != nil {
		log.Fatalf("Listener failed: %s", err)
	}
	defer listener.Close()

	fs := http.Server{Addr: ":" + PortStream, Handler: s}
	go func() {
		err := fs.ListenAndServe()
		if err != nil {
			log.Fatalf("File server failed: %s", err)
		}
	}()
	defer fs.Shutdown(context.Background())

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
		
		log.Successf("Client %d: connected", client.id)

		time, err := s.ipc.Request("get_property", "playback-time")
		if err != nil {
			log.Errorf("Client %d: failed to get playback time: %s, disconnecting", client.id, err)
			client.Close()
			continue
		}

		if !s.pause {
			s.dispatch(invalidID, &proto.Packet{Type: proto.PacketTypePause})
		}

		err = client.conn.WritePacket(&proto.Packet{
			Type: proto.PacketTypeInit,
			Payload: proto.EncodeInit(s.title, time.(float64), s.pause),
		})
		if err != nil {
			log.Errorf("Client %d: init packet failed: %s, disconnecting", client.id, err)
			client.Close()
			s.dispatch(invalidID, &proto.Packet{Type: proto.PacketTypeResume})
			continue
		}
		s.initCount += 1
		
		go client.handlePackets(s)
	}
}

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

func (s *Server) dispatch(except int, packet *proto.Packet) {
	if except != serverID {
		err := common.PacketToIPC(packet, s.debouncer, s.ipc)
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

	err = s.ipc.RegisterListener("seek", func(_ map[string]any) {
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
			Payload: proto.EncodeFloat64(time.(float64)),
		})
	})
	if err != nil {
		log.Errorf("Failed to register pause handler: %s", err)
	}
}
