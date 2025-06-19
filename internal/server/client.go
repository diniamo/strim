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

func (c *Client) handlePackets(s *Server) {
	var err error
	ready := false
	
	for {
		raw, err := c.conn.ReadRaw()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
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
		case proto.PacketTypeReady:
			// Technically shouldn't be needed, but just to make sure we don't compromise the server
			if !ready {
				log.Successf("Client %d: ready", c.id)
				
				s.initCount -= 1
				if s.initCount == 0 {
					
				}
				
				ready = true
			}
		}
		
		s.dispatchRaw(c.id, raw)
	}

	c.Close()

	if err == nil {
		log.Notef("Client %d: disconnected", c.id)
	} else {
		log.Warnf("Client %d: disconnected (%s)", c.id, err)
	}
}
