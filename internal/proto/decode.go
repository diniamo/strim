package proto

import "encoding/binary"

func DecodeFloat64(raw []byte) (ret float64) {
	binary.Decode(raw, binary.BigEndian, &ret)
	return
}

func DecodeInit(raw []byte) (title string, time float64, pause bool) {
	binary.Decode(raw[:8], binary.BigEndian, &time)
	binary.Decode(raw[8:9], binary.BigEndian, &pause)
	title = string(raw[9:])
	return
}
