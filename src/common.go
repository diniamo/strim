package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/diniamo/gopv"
	"github.com/eiannone/keyboard"
)

// SECTION: terminal
func readAnswer() byte {
	keyChan, err := keyboard.GetKeys(1)
	if err != nil { Fatal("Failed to setup terminal:", err) }

	key    := <-keyChan
	answer := byte(key.Rune)

	keyboard.Close()
	os.Stderr.Write([]byte{answer, '\n'})

	return answer
}

func readLine(prompt string) string {
	os.Stderr.Write([]byte(prompt))

	scanner := bufio.NewScanner(os.Stdin)
	ok      := scanner.Scan()
	if !ok { Fatal("Failed to read from stdin:", scanner.Err()) }

	return scanner.Text()
}

// SECTION: ports
const (
	messagePort uint16 = 1
	streamPort  uint16 = 2

	messageAddress = ":1"
	streamAddress  = ":2"
)

// SECTION: message protocol
const (
	headerSize     = 3
	maxMessageSize = headerSize + math.MaxUint16 + 1

	heartbeatInterval = 1 * time.Second
	heartbeatTimeout  = 3 * heartbeatInterval
)

var (
	// The byteOrder used for encoding and decoding message data.
	byteOrder = binary.LittleEndian

	errInvalidMessage = errors.New("invalid message")
)

type messageType byte
const (
	// The media should be paused.
	// No payload.
	messageTypePause messageType = iota
	// The media should be resumed.
	// No payload.
	messageTypeResume
	// The media should be sought.
	// Payload: time float64
	messageTypeSeek
	// The playback speed should be set.
	// Payload: speed float64
	messageTypeSpeed
	// The client should (re)load the media.
	// The url should be played if specified, otherwise the stream protocol should be used as an HTTP file server.
	// The client should also implicitly enter the waiting state.
	// Payload: url string, title string, time float64, speed float64
	messageTypePlay
	// The client has (re)loaded the media, and is ready to start playback.
	// This is also used after seeks.
	// No payload.
	messageTypeReady
	// The client should wait, as in it shouldn't allow the user to control the media.
	// Speed may still be changed.
	// Payload: time float64
	messageTypeWaitStart
	// The client should stop waiting, and resume the media, if it was playing before the wait started.
	// Payload: resume bool
	messageTypeWaitDone
	// The client sends this to the server periodically, otherwise it will be disconnected.
	// See heartbeatInterval and heartbeatTimeout.
	messageTypeHeartbeat

	// The number of mesage types.
	messageTypeCount
)

type invalidMessageTypeError struct {
	typ messageType
}
func (e *invalidMessageTypeError) Error() string {
	return fmt.Sprint("invalid message type: ", e.typ)
}

// A message.
type message struct {
	// The type of the message.
	typ messageType
	// The payload of the message.
	payload any
}

type messagePlay struct {
	// If the media is remote, url should be the URL, otherwise empty.
	// If the media is local (to the server), the title should be title of the media, otherwise empty.
	url, title  string
	// The initial time and speed of the playback.
	time, speed float64
}

var messageMarshallers = [...]func([]byte, any) int{
	messageTypePause: nilMarshaller,
	messageTypeResume: nilMarshaller,
	messageTypeSeek: func(data []byte, payload any) int {
		time := payload.(float64)
		binary.Encode(data, byteOrder, time)
		return 8
	},
	messageTypeSpeed: func(data []byte, payload any) int {
		speed := payload.(float64)
		binary.Encode(data, byteOrder, speed)
		return 8
	},
	messageTypePlay: func(data []byte, payload any) int {
		msg := payload.(messagePlay)

		i := 0
		var j int

		// url
		j = i + 2
		binary.Encode(data[i:j], byteOrder, uint16(len(msg.url)))
		i = j

		j = i + len(msg.url)
		copy(data[i:j], msg.url)
		i = j

		// title
		j = i + 2
		binary.Encode(data[i:j], byteOrder, uint16(len(msg.title)))
		i = j

		j = i + len(msg.title)
		copy(data[i:j], msg.title)
		i = j

		// time
		j = i + 8
		binary.Encode(data[i:j], byteOrder, msg.time)
		i = j

		// speed
		j = i + 8
		binary.Encode(data[i:j], byteOrder, msg.speed)
		i = j

		return i
	},
	messageTypeReady: nilMarshaller,
	messageTypeWaitStart: func(data []byte, payload any) int {
		time := payload.(float64)
		binary.Encode(data, byteOrder, time)
		return 8
	},
	messageTypeWaitDone: func(data []byte, payload any) int {
		resume := payload.(bool)
		binary.Encode(data, byteOrder, resume)
		return 1
	},
	messageTypeHeartbeat: nilMarshaller,
}

var messageUnmarshallers []func([]byte) any = []func([]byte) any{
	messageTypePause: nilUnmarshaller,
	messageTypeResume: nilUnmarshaller,
	messageTypeSeek: func(data []byte) any {
		var time float64
		binary.Decode(data, byteOrder, &time)
		return time
	},
	messageTypeSpeed: func(data []byte) any {
		var speed float64
		binary.Decode(data, byteOrder, &speed)
		return speed
	},
	messageTypePlay: func(data []byte) any {
		var msg messagePlay

		i := 0
		j := 0

		// url
		j = i + 2
		var urlLength uint16
		binary.Decode(data[i:j], byteOrder, &urlLength)
		i = j

		j = i + int(urlLength)
		msg.url = string(data[i:j])
		i = j

		// title
		j = i + 2
		var titleLength uint16
		binary.Decode(data[i:j], byteOrder, &titleLength)
		i = j

		j = i + int(titleLength)
		msg.title = string(data[i:j])
		i = j

		// time
		j = i + 8
		binary.Decode(data[i:j], byteOrder, &msg.time)
		i = j

		// speed
		j = i + 8
		binary.Decode(data[i:j], byteOrder, &msg.speed)
		i = j

		return msg
	},
	messageTypeReady: nilUnmarshaller,
	messageTypeWaitStart: func(data []byte) any {
		var time float64
		binary.Decode(data, byteOrder, &time)
		return time
	},
	messageTypeWaitDone: func(data []byte) any {
		var resume bool
		binary.Decode(data, byteOrder, &resume)
		return resume
	},
	messageTypeHeartbeat: nilUnmarshaller,
}

func nilMarshaller(_ []byte, _ any) int { return 0 }
func nilUnmarshaller(_ []byte)      any { return nil }

func marshalMessage(buf []byte, typ messageType, payload any) []byte {
	n := messageMarshallers[typ](buf[3:], payload)

	binary.Encode(buf[1:3], byteOrder, uint16(n))
	buf[0] = byte(typ)

	return buf[:3 + n]
}

func sendMessage(c net.Conn, typ messageType, payload any) (err error) {
	defer func() {
		if recover() != nil {
			err = errInvalidMessage
		}
	}()

	var buf [maxMessageSize]byte
	data := marshalMessage(buf[:], typ, payload)

	_, err = c.Write(data)
	return
}

func waitMessage(c net.Conn, data []byte) (typ messageType, payload any, err error) {
	defer func() {
		if recover() != nil {
			err = errInvalidMessage
		}
	}()

	_, err = io.ReadFull(c, data[:3])
	if err != nil { return }

	var size uint16
	binary.Decode(data[1:3], byteOrder, &size)

	payloadData := data[3:3 + size]
	_, err = io.ReadFull(c, payloadData)
	if err != nil { return }

	typ = messageType(data[0])
	if !isValidType(typ) {
		err = &invalidMessageTypeError{typ}
		return
	}
	payload = messageUnmarshallers[typ](payloadData)

	return
}

func isValidType(typ messageType) bool {
	return typ < messageTypeCount
}

func filterConnectionError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	} else {
		return err
	}
}

func unwrapOpError(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Err
	}
	return err
}

// SECTION: mpv
func setupPath() {
	strim, err := os.Executable()
	if err != nil {
		Warn("Failed to get executable path, can't check for bundled mpv:", err)
		return
	}

	dir := filepath.Dir(strim)
	old := os.Getenv("PATH")
	new := dir
	if old != "" {
		new += string(os.PathListSeparator) + old
	}

	os.Setenv("PATH", new)
}

func mpv(args ...string) (*exec.Cmd, *gopv.Client) {
	cmd := exec.Command("mpv", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	client, err := gopv.Host(cmd)
	if err != nil { Fatal("Failed to host mpv connection target:", err) }

	err = cmd.Start()
	if err != nil { Fatal("Failed to start mpv:", err) }

	return cmd, client
}

// SECTION: debouncer
// A debouncer used for ignoring events that are the result our own requests.
// A counter is not reliable for property-change events, because those aren't
// fired if you set the property to its current value, so we check for equality
// in the case of pause/resume and speed, and use a counter for seek, because
// seek events are always sent (even for in-place seeks).
type debouncer struct {
	pause bool
	speed float64
	seekCount int
}

// NOTE: mpv speed values are clamped to above 0.01.
// Add an extra 0 to be safe.
const epsilon float64 = 0.001

func (d *debouncer) debounceNext(typ messageType, payload any) {
	switch typ {
	case messageTypePause:
		d.pause = true
	case messageTypeResume:
		d.pause = false
	case messageTypeSpeed:
		d.speed = payload.(float64)
	case messageTypeSeek:
		d.seekCount += 1
	}
}

func (d *debouncer) debounceOrUpdate(typ messageType, payload any) bool {
	switch typ {
	case messageTypePause:
		old := d.pause
		d.pause = true
		return old
	case messageTypeResume:
		old := d.pause
		d.pause = false
		return !old
	case messageTypeSpeed:
		value := payload.(float64)
		if math.Abs(value - d.speed) > epsilon {
			d.speed = value
			return false
		} else {
			return true
		}
	case messageTypeSeek:
		if d.seekCount > 0 {
			d.seekCount -= 1
			return true
		} else {
			return false
		}
	}

	panic("unreachable")
}

// SECTION: overlay
const overlayID = 1
func showOverlay(ipc *gopv.Client, text string) {
	ipc.Request("osd-overlay", overlayID, "ass-events", `{\an9\i1\bord1}` + text)
}
func hideOverlay(ipc *gopv.Client) {
	ipc.Request("osd-overlay", overlayID, "none", "")
}
