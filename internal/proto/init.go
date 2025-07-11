package proto

import "encoding/binary"

func EncodeInit(title string, time float64) []byte {
	buf := make([]byte, 8 + len(title))

	binary.Encode(buf[:8], binary.BigEndian, time)

	// Encode the string last to avoid having to add the length
	for i := range len(title) {
		buf[8 + i] = title[i]
	}

	return buf
}

func DecodeInit(raw []byte) (title string, time float64) {
	binary.Decode(raw[:8], binary.BigEndian, &time)
	title = string(raw[8:])
	return
}
