package client

import (
	"errors"
	"io"
	"net"

	log "github.com/diniamo/glog"
	"github.com/diniamo/gopv"
	"github.com/diniamo/strim/internal/mpv"
	"github.com/diniamo/strim/internal/proto"
	"github.com/diniamo/strim/internal/server"
)

type Client struct {
	address string
	conn *proto.Conn

	ipc *gopv.Client
	debouncer mpv.Debouncer
}

func New(ipc *gopv.Client, address string) *Client {
	return &Client{
		address: net.JoinHostPort(address, server.Port),
		ipc: ipc,
		debouncer: make(mpv.Debouncer, 3),
	}
}

func (c *Client) Connect() error {
	conn, err := net.Dial("tcp", c.address)
	if err != nil {
		return err
	}

	_, err = conn.Write([]byte{server.MessageConnectionByte})
	if err != nil {
		return err
	}

	c.conn = proto.NewConn(conn)

	return nil
}

func (c *Client) PacketLoop() error {
	defer c.conn.Close()

	for {
		packet, err := c.conn.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				log.Warnf("Connection reset by server")
				break
			} else {
				log.Errorf("Failed to read packet: %s", err)
				continue
			}
		}

		switch packet.Type {
		case proto.PacketTypePause, proto.PacketTypeResume, proto.PacketTypeSeek:
			err = mpv.PacketToIPC(&packet, c.debouncer, c.ipc)
			if err != nil {
				log.Errorf("IPC request failed: %s", err)
			}
		case proto.PacketTypeInit:
			title, time := proto.DecodeInit(packet.Payload)

			log.Notef("Playing %s", title)

			err := c.load()
			if err != nil {
				log.Fatalf("Failed to connect to stream: %s", err)
			}

			_, err = c.ipc.Request("set_property", "title", title)
			if err != nil {
				log.Warnf("Failed to set title: %s", err)
			}

			if time != 0 {
				err = c.seekWait(time)
				if err != nil {
					log.Errorf("Initial seek failed: %s", err)
				}
			}
			
			err = c.conn.WritePacket(&proto.Packet{Type: proto.PacketTypeReady})
			if err == nil {
				log.Success("Ready")
			} else {
				log.Errorf("Failed to write ready packet: %s", err)
			}
		case proto.PacketTypeIdle:
			_, err := c.ipc.Request("playlist-play-index", "none")
			if err != nil {
				log.Warnf("Failed to go to idle state: %s", err)
			}
		}
	}

	return nil
}

func (c *Client) load() error {
	doneChan := make(chan struct{})
	defer close(doneChan)

	c.ipc.RegisterListener("file-loaded", func(_ map[string]any) {
		doneChan <- struct{}{}
	})

	_, err := c.ipc.Request("loadfile", "http://" + c.address)
	if err != nil {
		return err
	}

	<-doneChan
	c.ipc.UnregisterListener("file-loaded")

	return nil
}

func (c *Client) seekWait(time float64) error {
	doneChan := make(chan struct{})
	defer close(doneChan)
	
	c.ipc.RegisterListener("playback-restart", func(_ map[string]any) {
		doneChan <- struct{}{}
	})
	
	_, err := c.ipc.Request("set_property", "playback-time", time)
	if err != nil {
		return err
	}

	<-doneChan
	c.ipc.UnregisterListener("playback-restart")

	return nil
}

func (c *Client) RegisterHandlers() {
	_, err := c.ipc.ObserveProperty("pause", func(state any) {
		packetType := proto.PacketTypeResume
		if state.(bool) {
			packetType = proto.PacketTypePause
		}
		
		if c.debouncer.IsDebounce(packetType) {
			return
		}

		err := c.conn.WritePacket(&proto.Packet{Type: packetType})
		if err != nil {
			log.Errorf("Failed to send resume/pause packet: %s", err)
		}
	})
	if err != nil {
		log.Fatalf("Failed to register pause handler: %s", err)
	}

	c.ipc.RegisterListener("seek", func(_ map[string]any) {
		if c.debouncer.IsDebounce(proto.PacketTypeSeek) {
			return
		}

		time, err := c.ipc.Request("get_property", "playback-time")
		if err != nil {
			log.Errorf("Failed to get playback time: %s", err)
			return
		}

		err = c.conn.WritePacket(&proto.Packet{
			Type: proto.PacketTypeSeek,
			Payload: proto.EncodeSeek(time.(float64)),
		})
		if err != nil {
			log.Errorf("Failed to send seek packet: %s", err)
		}
	})
}
