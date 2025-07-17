package server

import (
	"net"
	"sync"

	log "github.com/diniamo/glog"
)

const MessageConnectionByte byte = '@'

type cMux struct {
	listener net.Listener
	message cMuxListener
	stream cMuxListener
	streamMu sync.Mutex
}

type cMuxConn struct {
	net.Conn
	b byte
	used bool
}

type cMuxListener struct {
	net.Listener
	connChan chan net.Conn
	doneChan chan struct{}
}

func newCMuxListener() cMuxListener {
	return cMuxListener{
		connChan: make(chan net.Conn),
		doneChan: make(chan struct{}),
	}
}

func (m *cMux) Serve() error {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			log.Warnf("Failed to accept connection: %s", err)
			continue
		}

		go func() {
			var buf [1]byte

			_, err := conn.Read(buf[:])
			if err != nil {
				log.Warnf("Decisive read failed: %s", err)
				return
			}

			if buf[0] == MessageConnectionByte {
				c := &cMuxConn{
					Conn: conn,
					used: true,
				}
				m.message.connChan <- c
			} else {
				c := &cMuxConn{
					Conn: conn,
					b: buf[0],
				}

				m.streamMu.Lock()
				m.stream.connChan <- c
				m.streamMu.Unlock()
			}
		}()
	}
}

func (l cMuxListener) Accept() (net.Conn, error) {
	select {
	case conn, ok := <-l.connChan:
		if ok {
			return conn, nil
		} else {
			return nil, net.ErrClosed
		}
	case <-l.doneChan:
		return nil, net.ErrClosed
	}
}

func (l cMuxListener) Close() error {
	select {
	case <-l.doneChan:
	default:
		l.doneChan <- struct{}{}
		close(l.doneChan)
		close(l.connChan)
	}
	
	return nil
}

func (c *cMuxConn) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	if c.used {
		return c.Conn.Read(buf)
	} else {
		buf[0] = c.b
		n, err := c.Conn.Read(buf[1:])
		return n + 1, err
	}
}

func (m *cMux) Close() {
	m.message.Close()
	m.streamMu.Lock()
	m.stream.Close()
	m.listener.Close()
}
