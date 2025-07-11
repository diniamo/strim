package mpv

import (
	"github.com/diniamo/gopv"
	"github.com/diniamo/strim/internal/proto"
)

func PacketToIPC(packet *proto.Packet, debouncer Debouncer, ipc *gopv.Client) (err error) {
	switch packet.Type {
	case proto.PacketTypePause:
		debouncer.Debounce(proto.PacketTypePause)
		_, err = ipc.Request("set_property", "pause", true)
	case proto.PacketTypeResume:
		debouncer.Debounce(proto.PacketTypeResume)
		_, err = ipc.Request("set_property", "pause", false)
	case proto.PacketTypeSeek:
		time := proto.DecodeSeek(packet.Payload)
		
		debouncer.Debounce(proto.PacketTypeSeek)
		_, err = ipc.Request("set_property", "playback-time", time)	
	}
	return
}
