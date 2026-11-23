package upstream

import (
	"io"
	"net"
	"time"
)

const (
	slabCount = 64
	slabSize  = 1024
	sendWait  = 100 * time.Millisecond
)

type slab struct {
	data [slabSize]byte
	n    int
}

var crlf = []byte{'\r', '\n'}

func (c *Conn) enqueue(line []byte) error {
	if len(line) > slabSize {
		return ErrLineTooLong
	}

	s := c.getSlab(sendWait)
	if s == nil {
		return ErrSendTimeout
	}
	s.n = copy(s.data[:], line)

	select {
	case c.writeSlabs <- s:
		return nil
	default:
		c.freeSlabs <- s
		return ErrSendTimeout
	}
}

func (c *Conn) getSlab(timeout time.Duration) *slab {
	select {
	case s := <-c.freeSlabs:
		return s
	default:
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case s := <-c.freeSlabs:
		return s
	case <-timer.C:
		return nil
	}
}

func (c *Conn) writeLoop(conn net.Conn, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case s := <-c.writeSlabs:
			err := writeAll(conn, s.data[:s.n])
			if err == nil {
				err = writeAll(conn, crlf)
			}
			c.freeSlabs <- s
			if err != nil {
				conn.Close()
				return
			}
			c.linesOut.Add(1)
			c.bytesOut.Add(uint64(s.n) + 2) // + CRLF
		}
	}
}

func (c *Conn) drainWrites() {
	for {
		select {
		case s := <-c.writeSlabs:
			c.freeSlabs <- s
		default:
			return
		}
	}
}

func (c *Conn) writeLine(conn net.Conn, line []byte) error {
	buf := make([]byte, len(line)+2)
	copy(buf, line)
	buf[len(line)] = '\r'
	buf[len(line)+1] = '\n'
	if err := writeAll(conn, buf); err != nil {
		return err
	}
	c.linesOut.Add(1)
	c.bytesOut.Add(uint64(len(buf)))
	return nil
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}
