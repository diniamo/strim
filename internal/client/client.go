package client

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"

	log "github.com/diniamo/glog"
	"github.com/diniamo/gopv"
	"github.com/diniamo/rife/internal/common"
	"github.com/diniamo/rife/internal/mpv"
	"github.com/diniamo/rife/internal/proto"
	"github.com/diniamo/rife/internal/server"
)

type Client struct {
	address string
	conn *proto.Conn
	debouncer mpv.Debouncer

	mpv *exec.Cmd
	ipc *gopv.Client
}

func Connect(address string) (*Client, error) {
	conn, err := net.Dial("tcp", fmt.Sprintf("%s:%s", address, server.PortMessage))
	if err != nil {
		return nil, err
	}

	return &Client{
		address: address,
		conn: proto.NewConn(conn),
		debouncer: make(mpv.Debouncer, 3),
	}, nil
}

func (c *Client) PacketLoop() error {
	defer c.conn.Close()

	ready := false
	for {
		packet, err := c.conn.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				log.Warnf("Disconnected")
				break
			} else {
				log.Errorf("Failed to read packet: %s", err)
				continue
			}
		}

		switch packet.Type {
		case proto.PacketTypePause, proto.PacketTypeResume, proto.PacketTypeSeek:
			if ready {
				err = common.PacketToIPC(&packet, c.debouncer, c.ipc)
				if err != nil {
					log.Warnf("IPC request failed: %s", err)
				}
			}
		case proto.PacketTypeInit:
			title, time, pause := proto.DecodeInit(packet.Payload)

			log.Notef("Playing %s", title)

			c.mpv, c.ipc, err = mpv.Open(
				"--force-media-title=" + title,
				"--no-resume-playback", "--no-save-position-on-quit",
				"--pause",
				fmt.Sprintf("http://%s:%s", c.address, server.PortStream),
			)
			if err != nil {
				return errors.New("Failed to open mpv: " + err.Error())
			}
			go func() {
				err := c.mpv.Wait()
				if err == nil {
					os.Exit(0)
				} else {
					log.Warnf("Mpv exited with an error: %s", err)
					os.Exit(1)
				}
			}()

			c.registerHandlers()

			err = c.SeekWait(time)
			if err != nil {
				log.Errorf("Initial seek failed: %s", err)
				continue
			}

			if !pause {
				_, err = c.ipc.Request("set_property", "pause", false)
				if err != nil {
					log.Errorf("Initial resume failed: %s", err)
				}
			}
			
			err = c.conn.WritePacket(&proto.Packet{Type: proto.PacketTypeReady})
			if err != nil {
				log.Errorf("Ready packet failed: %s", err)
			}

			ready = true
		}
	}

	return nil
}

func (c *Client) waitEvent(event string) error {
	doneChan := make(chan struct{})
	defer close(doneChan)

	err := c.ipc.RegisterListener(event, func(_ map[string]any) {
		doneChan <- struct{}{}
	})
	if err != nil {
		return err
	}

	<-doneChan
	c.ipc.UnregisterListener(event)

	return nil
}

func (c *Client) SeekWait(time float64) error {
	_, err := c.ipc.Request("set_property", "playback-time", time)
	if err != nil {
		return err
	}

	err = c.waitEvent("playback-restart")
	if err != nil {
		return err
	}

	return nil
}

func (c *Client) registerHandlers() {
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

	err = c.ipc.RegisterListener("seek", func(_ map[string]any) {
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
			Payload: proto.EncodeFloat64(time.(float64)),
		})
		if err != nil {
			log.Errorf("Failed to send seek packet: %s", err)
		}
	})
	if err != nil {
		log.Fatalf("Failed to register seek handler: %s", err)
	}
}
