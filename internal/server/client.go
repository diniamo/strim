package server

import (
	"bufio"
	"errors"
	"io"

	log "github.com/diniamo/glog"
	"github.com/diniamo/strim/internal/common"
	"github.com/diniamo/strim/internal/proto"
)

type Client struct {
	id int
	alive bool
	conn *proto.Conn
	reader *bufio.Reader
}

func (c *Client) Close() {
	c.alive = false
	c.conn.Close()
}

func (c *Client) packetLoop(s *Server) {
	for {
		raw, err := c.conn.ReadRaw()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Warnf("Client %d: read error: %s", c.id, err)
			}
			
			break
		}

		packet := proto.DecodePacket(raw)

		switch packet.Type {
		case proto.PacketTypePause, proto.PacketTypeResume, proto.PacketTypeSeek:
			err = common.PacketToIPC(&packet, s.debouncer, s.ipc)
			if err != nil {
				log.Errorf("IPC request failed: %s", err)
			}
			
			s.dispatchRaw(c.id, raw)
		case proto.PacketTypeReady:
			log.Successf("Client %d: ready", c.id)
				
			s.initCount -= 1
			if s.initCount == 0 {
				s.dispatch(invalidID, &proto.Packet{Type: proto.PacketTypeResume})
			}
		}
	}

	c.Close()

	log.Notef("Client %d: disconnected", c.id)
}
