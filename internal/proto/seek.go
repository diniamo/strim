package proto

import "encoding/binary"

func EncodeSeek(time float64) []byte {
	buf := make([]byte, 8)
	binary.Encode(buf, binary.BigEndian, time)
	return buf
}

func DecodeSeek(raw []byte) (time float64) {
	binary.Decode(raw, binary.BigEndian, &time)
	return
}
