package upstream

import (
	"bufio"
	"io"
	"time"

	"github.com/iakat/salotto/internal/ircline"
)

func (c *Conn) readLoop(br *bufio.Reader, scratch []byte) error {
	var pongBuf [512]byte
	for {
		line, err := readLine(br, scratch)
		if err != nil {
			return err
		}

		_, rest := ircline.Tags(line)
		_, rest = ircline.Prefix(rest)
		if ircline.IsVerb(rest, "PING") {
			params := ircline.Params(rest)
			var pong []byte
			if len(params)+5 <= len(pongBuf) {
				n := copy(pongBuf[:], "PONG ")
				n += copy(pongBuf[n:], params)
				pong = pongBuf[:n]
			} else {
				pong = append([]byte("PONG "), params...)
			}
			if err := c.enqueue(pong); err != nil {
				return err
			}
			continue
		}

		seq := c.cfg.Ring.AppendStamp(line, time.Now().UnixNano())
		c.linesIn.Add(1)
		if c.cfg.OnLine != nil {
			c.cfg.OnLine(seq, line)
		}
	}
}

func readLine(br *bufio.Reader, scratch []byte) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		n := len(line)
		if n > maxLine {
			n = maxLine
		}
		copy(scratch[:n], line[:n])
		for {
			_, derr := br.ReadSlice('\n')
			switch derr {
			case nil:
				return trimEOL(scratch[:n]), nil
			case bufio.ErrBufferFull:
				continue
			case io.EOF:
				return trimEOL(scratch[:n]), nil
			default:
				return nil, derr
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if len(line) > maxLine {
		line = line[:maxLine]
	}
	return trimEOL(line), nil
}

func trimEOL(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	return b
}
