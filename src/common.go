package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/eiannone/keyboard"

	p2p "github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/routing"
	circuitproto "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/proto"

	"github.com/diniamo/gopv"
)

// NOTE: terminal
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

// NOTE: P2P
const (
	dhtTimeout = 5 * time.Second
	messageProtocol   = protocol.ID("/strim/message/1")
	streamProtocol    = protocol.ID("/strim/stream/1")
)

func createHost() (host.Host, *dht.IpfsDHT) {
	var idht *dht.IpfsDHT

	routingConfigurator := func(h host.Host) (routing.PeerRouting, error) {
		instance, err := dht.New(context.Background(), h, dht.Mode(dht.ModeAuto))
		if err != nil { return nil, err }

		idht = instance
		return instance, err
	}

	peerSource := func(ctx context.Context, num int) <-chan peer.AddrInfo {
		ch := make(chan peer.AddrInfo)

		go func() {
			defer close(ch)

			h     := idht.Host()
			store := h.Peerstore()

			for _, node := range idht.RoutingTable().ListPeers() {
				supported, err := store.SupportsProtocols(node, circuitproto.ProtoIDv2Hop)
				if err != nil || len(supported) == 0 { continue }

				addrs := store.Addrs(node)
				info  := peer.AddrInfo{ID: node, Addrs: addrs}

				select {
				case ch <- info:
					break
				case <-ctx.Done():
					return
				}
			}
		}()

		return ch
	}

	// NOTE: create host
	h, err := p2p.New(
		p2p.NATPortMap(),
		p2p.EnableHolePunching(),
		p2p.EnableAutoNATv2(),
		p2p.Routing(routingConfigurator),
		p2p.EnableAutoRelayWithPeerSource(peerSource),
	)
	if err != nil { Fatal("Failed to create P2P host:", err) }

	// NOTE: bootstrap DHT
	err = idht.Bootstrap(context.Background())
	if err != nil { Fatal("DHT boostrap failed:", err) }

	peers, err := peer.AddrInfosFromP2pAddrs(dht.DefaultBootstrapPeers...)
	if err != nil { Fatal("Failed to convert P2P addresses to infos:", err) }

	var wg sync.WaitGroup
	for _, info := range peers {
		wg.Add(1)

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), dhtTimeout)

			err = h.Connect(ctx, info)
			if err != nil { Warnf("Bootstrap peer %s unreachable: %s", info.ID, err) }

			cancel()
			wg.Done()
		}()
	}
	wg.Wait()

	return h, idht
}


// NOTE: message protocol
// The byteOrder used for encoding and decoding message data.
var byteOrder = binary.LittleEndian

type messageType byte
// IMPORTANT: the message types that may be debounced must be kept at the start.
const (
	// The media should be paused.
	// No payload.
	messageTypePause = iota
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
)

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

var messageMarshallers []func([]byte, any) int = []func([]byte, any) int{
	nilMarshaller,
	nilMarshaller,
	func(data []byte, payload any) int {
		time := payload.(float64)
		binary.Encode(data, byteOrder, time)
		return 8
	},
	func(data []byte, payload any) int {
		speed := payload.(float64)
		binary.Encode(data, byteOrder, speed)
		return 8
	},
	func(data []byte, payload any) int {
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
	nilMarshaller,
	func(data []byte, payload any) int {
		time := payload.(float64)
		binary.Encode(data, byteOrder, time)
		return 8
	},
	func(data []byte, payload any) int {
		resume := payload.(bool)
		binary.Encode(data, byteOrder, resume)
		return 1
	},
}

var messageUnmarshallers []func([]byte) any = []func([]byte) any{
	nilUnmarshaller,
	nilUnmarshaller,
	func(data []byte) any {
		var time float64
		binary.Decode(data, byteOrder, &time)
		return time
	},
	func(data []byte) any {
		var speed float64
		binary.Decode(data, byteOrder, &speed)
		return speed
	},
	func(data []byte) any {
		var msg messagePlay

		i := 0
		var j int

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
	nilUnmarshaller,
	func(data []byte) any {
		var time float64
		binary.Decode(data, byteOrder, &time)
		return time
	},
	func(data []byte) any {
		var resume bool
		binary.Decode(data, byteOrder, &resume)
		return resume
	},
}

func nilMarshaller(_ []byte, _ any) int { return 0 }
func nilUnmarshaller(_ []byte)      any { return nil }

func sendMessage(w io.Writer, typ messageType, payload any) {
	var data [4096]byte

	n := messageMarshallers[typ](data[3:], payload)

	binary.Encode(data[1:3], byteOrder, uint16(n))
	data[0] = byte(typ)

	w.Write(data[:3 + n])
}

func waitMessage(r io.Reader, data []byte) (messageType, any, error) {
	err := readExact(r, data[:3])
	if err != nil { return 0, nil, err }

	var size uint16
	binary.Decode(data[1:3], byteOrder, &size)

	payloadData := data[3:3 + size]
	err = readExact(r, payloadData)
	if err != nil { return 0, nil, err }

	typ     := data[0]
	payload := messageUnmarshallers[typ](payloadData)

	return messageType(typ), payload, nil
}

func readExact(r io.Reader, data []byte) error {
	n := 0
	for n < len(data) {
		read, err := r.Read(data[n:])
		if err != nil {
			return err
		}

		n += read
	}

	return nil
}

// NOTE: mpv
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
	// cmd.Stdout = os.Stdout
	// cmd.Stderr = os.Stderr

	client, err := gopv.Host(cmd)
	if err != nil { Fatal("Failed to host mpv connection target:", err) }

	err = cmd.Start()
	if err != nil { Fatal("Failed to start mpv:", err) }

	return cmd, client
}

// NOTE: debouncer
// A debouncer used for ignoring events that were caused by our own requests.
type debouncer []int

// The number of message types that may be debounced.
// IMPORTANT: this has to be kept up to date.
const debounceTypes = 4

func newDebouncer() debouncer {
	return make(debouncer, debounceTypes)
}

func (d debouncer) push(typ messageType) {
	d[typ] += 1
}

func (d debouncer) pop(typ messageType) bool {
	n := d[typ]
	if n > 0 {
		d[typ] = n - 1
		return true
	} else {
		return false
	}
}
