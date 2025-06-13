package common

import (
	"github.com/diniamo/gopv"
	"github.com/diniamo/rife/internal/mpv"
	"github.com/diniamo/rife/internal/proto"
)

func PacketToIPC(packet *proto.Packet, debouncer mpv.Debouncer, ipc *gopv.Client) (err error) {
	switch packet.Type {
	case proto.PacketTypePause:
		debouncer.Debounce(proto.PacketTypePause)
		_, err = ipc.Request("set_property", "pause", true)
	case proto.PacketTypeResume:
		debouncer.Debounce(proto.PacketTypeResume)
		_, err = ipc.Request("set_property", "pause", false)
	case proto.PacketTypeSeek:
		time := proto.DecodeFloat64(packet.Payload)
		
		debouncer.Debounce(proto.PacketTypeSeek)
		_, err = ipc.Request("set_property", "playback-time", time)	
	}
	return
}
