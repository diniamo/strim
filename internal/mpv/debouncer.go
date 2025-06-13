package mpv

import (
	"time"

	"github.com/diniamo/rife/internal/proto"
)

const threshold = 100 * time.Millisecond

type Debouncer map[proto.PacketType]time.Time

func (d Debouncer) Debounce(key proto.PacketType) {
	d[key] = time.Now()
}

func (d Debouncer) IsDebounce(key proto.PacketType) bool {
	return time.Since(d[key]) < threshold
}
