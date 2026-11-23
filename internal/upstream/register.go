package upstream

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/iakat/salotto/internal/ircline"
)

func (c *Conn) register(conn net.Conn, br *bufio.Reader, scratch []byte) error {
	if err := c.writeLine(conn, []byte("CAP LS 302")); err != nil {
		return err
	}

	var offered []string
	for {
		line, err := readLine(br, scratch)
		if err != nil {
			return err
		}
		_, rest := ircline.Tags(line)
		_, rest = ircline.Prefix(rest)
		if ircline.IsVerb(rest, "PING") {
			if err := c.answerPing(conn, rest); err != nil {
				return err
			}
			continue
		}
		if !ircline.IsVerb(rest, "CAP") {
			continue
		}
		caps, more, ok := parseCapLS(ircline.Params(rest))
		if !ok {
			continue
		}
		offered = append(offered, caps...)
		if !more {
			break
		}
	}

	want, sasl := selectCaps(offered, c.cfg.saslWanted())
	c.SetCaps(want)
	if len(want) > 0 {
		if err := c.writeLine(conn, []byte("CAP REQ :"+strings.Join(want, " "))); err != nil {
			return err
		}
	}

	if sasl {
		mech := "PLAIN"
		if c.cfg.SASLExternal {
			mech = "EXTERNAL"
		}
		if err := c.writeLine(conn, []byte("AUTHENTICATE "+mech)); err != nil {
			return err
		}
		if err := c.waitAuthenticate(conn, br, scratch); err != nil {
			return err
		}
		// EXTERNAL authenticates with the TLS client certificate, so the
		// response is empty: ZNC sends a single "+" for every mechanism other
		// than PLAIN (sasl.cpp:198-213), and the server reads the identity from
		// the certificate.
		payload := "+"
		if !c.cfg.SASLExternal {
			payload = base64.StdEncoding.EncodeToString([]byte(c.cfg.SASLUser + "\x00" + c.cfg.SASLUser + "\x00" + c.cfg.SASLPass))
		}
		if err := c.writeLine(conn, []byte("AUTHENTICATE "+payload)); err != nil {
			return err
		}
		if err := c.waitSASLResult(br, scratch); err != nil {
			return err
		}
	}

	if err := c.writeLine(conn, []byte("CAP END")); err != nil {
		return err
	}
	if err := c.writeLine(conn, []byte("NICK "+c.cfg.Nick)); err != nil {
		return err
	}
	if err := c.writeLine(conn, []byte("USER "+c.cfg.User+" 0 * :"+c.cfg.RealName)); err != nil {
		return err
	}

	for {
		line, err := readLine(br, scratch)
		if err != nil {
			return err
		}
		_, rest := ircline.Tags(line)
		_, rest = ircline.Prefix(rest)
		if ircline.IsVerb(rest, "PING") {
			if err := c.answerPing(conn, rest); err != nil {
				return err
			}
			continue
		}
		if ircline.IsVerb(rest, "001") {
			return nil
		}
	}
}

func (c *Conn) waitAuthenticate(conn net.Conn, br *bufio.Reader, scratch []byte) error {
	for {
		line, err := readLine(br, scratch)
		if err != nil {
			return err
		}
		_, rest := ircline.Tags(line)
		_, rest = ircline.Prefix(rest)
		if ircline.IsVerb(rest, "PING") {
			if err := c.answerPing(conn, rest); err != nil {
				return err
			}
			continue
		}
		if ircline.IsVerb(rest, "AUTHENTICATE") {
			if bytes.Equal(bytes.TrimSpace(ircline.Params(rest)), []byte("+")) {
				return nil
			}
			continue
		}
		if ircline.IsVerb(rest, "CAP") {
			toks := splitParams(ircline.Params(rest))
			if len(toks) >= 2 && bytes.EqualFold(toks[1], []byte("NAK")) {
				return errors.New("upstream: SASL capability refused")
			}
		}
	}
}

func (c *Conn) waitSASLResult(br *bufio.Reader, scratch []byte) error {
	for {
		line, err := readLine(br, scratch)
		if err != nil {
			return err
		}
		_, rest := ircline.Tags(line)
		_, rest = ircline.Prefix(rest)
		verb := ircline.Verb(rest)
		switch {
		case bytes.Equal(verb, []byte("903")):
			return nil
		case bytes.Equal(verb, []byte("904")),
			bytes.Equal(verb, []byte("905")),
			bytes.Equal(verb, []byte("906")),
			bytes.Equal(verb, []byte("907")):
			return fmt.Errorf("upstream: SASL failed (%s)", verb)
		}
	}
}
