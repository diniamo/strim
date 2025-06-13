package proto

import (
	"encoding/binary"
)

func EncodeFloat64(f float64) []byte {
	buf := make([]byte, 8)
	binary.Encode(buf, binary.BigEndian, f)
	return buf
}

func EncodeInit(title string, time float64, pause bool) []byte {
	buf := make([]byte, 8 + 1 + len(title))

	binary.Encode(buf[:8], binary.BigEndian, time)
	binary.Encode(buf[8:9], binary.BigEndian, pause)

	// Encode the string last to avoid having to add the length
	for i := range len(title) {
		buf[9 + i] = title[i]
	}

	return buf
}
