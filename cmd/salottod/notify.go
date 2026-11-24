package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/iakat/salotto/internal/push"
)

// notifier posts mentions to the web front end's /push/notify endpoint.
//
// Why it is an HTTP call and not an in-process call: the bouncer and the front end are separate
// pods in this deployment and separate processes everywhere else. An in-process design would
// work only in the configuration where they happen to be co-located, which is the configuration
// that never has to send a push because the user is looking at the screen.
type notifier struct {
	// log reports sends and failures. A notification path with no output is
	// indistinguishable from one that never fires: found by trying to trigger a real push
	// against the deployment and having nothing anywhere to say whether the bouncer asked,
	// whether the front end sent, or whether the push service refused. Every branch below
	// has something to say, and the failure branch is the one that matters most.
	log   func(msg string, args ...any)
	url   string
	token string
	cli   *http.Client

	mu     sync.Mutex
	recent map[string]time.Time // notification tag -> when we last sent it
	// window is how long a tag is suppressed for. One channel burst is one notification: a
	// phone that buzzes for every line of a conversation is a phone whose notifications get
	// turned off, and then a real mention is missed too.
	window time.Duration
}

// newNotifier builds a notifier, or nil if no endpoint is configured.
func newNotifier(url, token string) *notifier {
	if url == "" {
		return nil
	}
	return &notifier{
		url:    url,
		token:  token,
		cli:    &http.Client{Timeout: 5 * time.Second},
		recent: make(map[string]time.Time),
		window: 30 * time.Second,
	}
}

// notify sends one notification, unless the same tag was sent inside the suppression window.
//
// It is called from the dispatch path and must never block it: the send happens on its own
// goroutine and every error is dropped. A push that fails is a missed buzz; a dispatch path
// that waits for a phone is a bouncer that stops delivering lines.
func (n *notifier) notify(msg push.Message) {
	if n == nil {
		return
	}
	if msg.Tag == "" {
		msg.Tag = "salotto"
	}
	n.mu.Lock()
	if at, ok := n.recent[msg.Tag]; ok && time.Since(at) < n.window {
		n.mu.Unlock()
		return
	}
	n.recent[msg.Tag] = time.Now()
	// The map is bounded by the number of windows, but a long-lived daemon with many queries
	// would grow it forever; drop anything older than an hour on each send.
	for tag, at := range n.recent {
		if time.Since(at) > time.Hour {
			delete(n.recent, tag)
		}
	}
	n.mu.Unlock()

	body, err := json.Marshal(msg)
	if err != nil {
		n.note("push: cannot encode the message", "err", err)
		return
	}
	go func() {
		req, err := http.NewRequest(http.MethodPost, n.url, bytes.NewReader(body))
		if err != nil {
			n.note("push: bad request for the notifier", "url", n.url, "err", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if n.token != "" {
			req.Header.Set("Authorization", "Bearer "+n.token)
		}
		resp, err := n.cli.Do(req)
		if err != nil {
			n.note("push: the notifier did not answer", "url", n.url, "err", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			// A 403 here is a token mismatch, a 503 is push not configured on the front end:
			// both look like "the phone is quiet" from outside, which is why the status is
			// worth a line in the log.
			n.note("push: the notifier refused the message", "url", n.url, "status", resp.StatusCode)
			return
		}
		n.note("push: notification sent", "tag", msg.Tag, "title", msg.Title)
	}()
}

// note writes a line if a logger is installed. Nil-safe, because a notifier built without one
// is still a working notifier - the logging is for the operator, not for the send.
func (n *notifier) note(msg string, args ...any) {
	if n == nil || n.log == nil {
		return
	}
	n.log(msg, args...)
}

// mentionMessage builds the notification for a line that addressed us.
//
// The body is the line itself, truncated: a notification is read on a lock screen, and a
// channel name plus a nick plus a 400-character message is a notification nobody finishes.
// The window is the tag, so a burst in one channel replaces itself instead of stacking.
func mentionMessage(network, window, nick, text string) push.Message {
	preview := strings.TrimSpace(text)
	const limit = 180
	if len(preview) > limit {
		// Cut on a rune boundary: a notification cut mid-rune shows a replacement character,
		// which looks like the bouncer is broken rather than the message being long.
		cut := limit
		for cut > 0 && !utf8Start(preview[cut]) {
			cut--
		}
		preview = preview[:cut] + "…"
	}
	title := window
	if network != "" && network != window {
		title = window + " (" + network + ")"
	}
	if nick != "" {
		preview = nick + ": " + preview
	}
	return push.Message{
		Title: title,
		Body:  preview,
		Tag:   network + "/" + window,
	}
}

// utf8Start reports whether b starts a UTF-8 sequence, so a truncation can back up to one.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// parseMention pulls the pieces out of an IRC line, for the notification body.
//
// It is deliberately forgiving: a line that does not parse produces empty fields and a
// notification with a window name and no text, which is still true, rather than a skipped
// notification or a panic in the dispatch path.
func parseMention(line string) (nick, text string) {
	// IRCv3 message tags come first when they are present: "@time=...;msgid=... :nick!u@h
	// PRIVMSG #chan :text". Every line from a tag-capable upstream has them, so a parser that
	// insists on a leading ":" sees no mention on any network that speaks message-tags - which
	// is most of them, and was all of them here. Found live: the notifier had never once
	// fired, and the unit tests passed because their fixtures were untagged.
	if strings.HasPrefix(line, "@") {
		space := strings.IndexByte(line, ' ')
		if space < 0 {
			return "", ""
		}
		line = line[space+1:]
	}
	// :nick!user@host PRIVMSG #chan :text
	if !strings.HasPrefix(line, ":") {
		return "", ""
	}
	rest := line[1:]
	space := strings.IndexByte(rest, ' ')
	if space < 0 {
		return "", ""
	}
	prefix := rest[:space]
	if bang := strings.IndexByte(prefix, '!'); bang >= 0 {
		prefix = prefix[:bang]
	}
	body := rest[space+1:]
	// Anything before the first " :" is parameters; the trailing one is the message.
	idx := strings.Index(body, " :")
	if idx < 0 {
		return prefix, ""
	}
	return prefix, body[idx+2:]
}

// onMention decides whether a line addressed us and, if so, notifies.
//
// The mention test is the same one the TUI uses - our nick as a whole word - and it lives here
// rather than in the transport because this is the only place that knows whether anybody is
// watching. A mention while a client is attached is a line on screen; a mention while nobody is
// attached is the case the whole feature exists for.
func (n *notifier) onMention(network, window, line, ownNick string, attached bool) {
	if n == nil || attached || ownNick == "" {
		return
	}
	nick, text := parseMention(line)
	if nick == "" || nick == ownNick {
		return // a server line, or our own echo
	}
	if !mentionsNick(text, ownNick) {
		return
	}
	n.notify(mentionMessage(network, window, nick, text))
}

// mentionsNick reports whether text addresses nick as a word.
//
// Not a substring: "alice" in "alicephones" is not a mention, and a notification that fires on
// every line containing a common nick is a notification nobody keeps. The check is on the whole
// token, with IRC's nick characters as the separators.
func mentionsNick(text, nick string) bool {
	if nick == "" {
		return false
	}
	lowerText := strings.ToLower(text)
	lowerNick := strings.ToLower(nick)
	for i := 0; i+len(lowerNick) <= len(lowerText); i++ {
		if lowerText[i:i+len(lowerNick)] != lowerNick {
			continue
		}
		// The character before must not be a nick character.
		if i > 0 && isNickChar(lowerText[i-1]) {
			continue
		}
		end := i + len(lowerNick)
		if end < len(lowerText) && isNickChar(lowerText[end]) {
			continue
		}
		return true
	}
	return false
}

// isNickChar is the set IRC allows in a nick, plus the punctuation that makes "alice:" a
// mention and "alice's" arguably one.
func isNickChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '-' || b == '_' || b == '[' || b == ']' || b == '\\' || b == '`' || b == '^' || b == '{' || b == '}':
		return true
	}
	return false
}
