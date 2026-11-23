package upstream

import (
	"bufio"
	"bytes"
	"io"

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

		c.trackSelf(rest)
		c.linesIn.Add(1)
		c.bytesIn.Add(uint64(len(line)))
		c.store(line, false)
	}
}

// trackSelf keeps the nick the server actually gave us. ZNC needs the same value
// for keepnick ("do we already have the nick we want?"), and so does anything
// that has to tell our own messages from someone else's.
func (c *Conn) trackSelf(rest []byte) {
	if ircline.IsVerb(rest, "001") {
		if f := bytes.Fields(ircline.Params(rest)); len(f) > 0 {
			c.selfNick.Store(string(f[0]))
		}
		return
	}
	if !ircline.IsVerb(rest, "NICK") {
		c.trackMask(rest)
		return
	}
	_, pfx := ircline.Tags(rest)
	pfx, _ = ircline.Prefix(pfx)
	from := ircline.Nick(pfx)
	if from == "" || from != c.selfNick.Load() {
		return
	}
	if f := bytes.Fields(ircline.Params(rest)); len(f) > 0 {
		c.selfNick.Store(string(f[0]))
	}
}

// trackMask records our own nick!ident@host from any line whose prefix is us.
// The 001 only carries the nick, so the mask has to be learned from the first
// line we send-or-receive under it: ZNC does the same by constructing a CNick
// out of every message, and LISTNETWORKS prints the result.
func (c *Conn) trackMask(rest []byte) {
	mask := c.selfMask.Load()
	if s, ok := mask.(string); ok && s != "" {
		return
	}
	_, pfx := ircline.Tags(rest)
	pfx, _ = ircline.Prefix(pfx)
	if len(pfx) == 0 {
		return
	}
	from := ircline.Nick(pfx)
	self := c.CurrentNick()
	if from == "" || self == "" || from != self {
		return
	}
	if bytes.IndexByte(pfx, '@') < 0 {
		return
	}
	c.selfMask.Store(string(pfx))
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
