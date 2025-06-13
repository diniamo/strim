package proto

type PacketType byte
const (
	PacketTypeInit PacketType = iota
	PacketTypeReady
	PacketTypePause
	PacketTypeResume
	PacketTypeSeek
)

type PacketSize uint16

type Packet struct {
	Type PacketType
	Payload []byte
}

func EncodePacket(packet *Packet, buf []byte) []byte {
	buf[0] = byte(packet.Type)
	for i, b := range packet.Payload {
		buf[1 + i] = b
	}

	return buf[:1 + len(packet.Payload)]
}

func DecodePacket(raw []byte) Packet {
	return Packet{
		Type: PacketType(raw[0]),
		Payload: raw[1:],
	}
}
