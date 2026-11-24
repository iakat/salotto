package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iakat/salotto/internal/facade"
	"github.com/iakat/salotto/internal/push"
)

// TestParseMentionPullsTheUsefulParts: the notification body comes from the line, and the line
// is whatever the network sent.
func TestParseMentionPullsTheUsefulParts(t *testing.T) {
	nick, text := parseMention(":alice!a@host PRIVMSG #chan :salotto: are you there?")
	if nick != "alice" {
		t.Fatalf("nick = %q", nick)
	}
	if text != "salotto: are you there?" {
		t.Fatalf("text = %q", text)
	}
}

// TestParseMentionIsForgiving: a line that does not parse must produce empty fields, not a
// panic. This runs on the dispatch path, so a panic here is every incoming line.
func TestParseMentionIsForgiving(t *testing.T) {
	for _, line := range []string{
		"",
		"garbage",
		":",
		":nickonly",
		":nick!u@h",
		":nick!u@h PRIVMSG #chan",
		"PRIVMSG #chan :no prefix",
	} {
		nick, text := parseMention(line)
		_ = nick
		_ = text
	}
}

// TestMentionMessageNamesTheWindowAndTruncates: a notification is read on a lock screen, and a
// 400-character message is one nobody finishes.
func TestMentionMessageNamesTheWindowAndTruncates(t *testing.T) {
	long := strings.Repeat("x", 400)
	msg := mentionMessage("libera", "#chan", "alice", long)
	if msg.Title != "#chan (libera)" {
		t.Fatalf("title = %q", msg.Title)
	}
	if len([]rune(msg.Body)) > 200 {
		t.Fatalf("body is %d runes", len([]rune(msg.Body)))
	}
	if !strings.HasPrefix(msg.Body, "alice: ") {
		t.Fatalf("body does not name the sender: %q", msg.Body)
	}
	if !strings.HasSuffix(msg.Body, "…") {
		t.Fatalf("a truncated body has no ellipsis: %q", msg.Body)
	}
	if msg.Tag != "libera/#chan" {
		t.Fatalf("tag = %q", msg.Tag)
	}
}

// TestTruncationBacksUpToARuneBoundary: cutting mid-rune shows a replacement character, which
// looks like the bouncer is broken rather than the message being long.
func TestTruncationBacksUpToARuneBoundary(t *testing.T) {
	// 179 ASCII bytes then a 3-byte rune straddling the 180 limit.
	body := strings.Repeat("a", 179) + "€" + strings.Repeat("b", 50)
	msg := mentionMessage("", "#c", "", body)
	// The ellipsis must follow valid UTF-8; a mid-rune cut would leave an invalid byte that
	// strings.ToValidUTF8 reports as U+FFFD.
	if got := strings.ToValidUTF8(msg.Body, "?"); got != msg.Body {
		t.Fatalf("the truncated body is not valid UTF-8: %q", msg.Body)
	}
}

// TestTheNotifierPostsWithTheToken is the whole path from a mention to a request.
func TestTheNotifierPostsWithTheToken(t *testing.T) {
	// The handler runs on the server's goroutine, so anything it writes and the test reads
	// needs a channel between them. A mutex would do, but a channel is also the wait.
	type captured struct {
		auth string
		msg  push.Message
	}
	gotc := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m push.Message
		_ = json.NewDecoder(r.Body).Decode(&m)
		gotc <- captured{auth: r.Header.Get("Authorization"), msg: m}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := newNotifier(srv.URL, "sekrit")
	if n == nil {
		t.Fatal("no notifier built for a configured URL")
	}
	n.notify(mentionMessage("libera", "#chan", "alice", "hello"))

	// The send is asynchronous; the channel is both the wait and the memory barrier.
	select {
	case got := <-gotc:
		if got.auth != "Bearer sekrit" {
			t.Fatalf("Authorization = %q", got.auth)
		}
		if got.msg.Title != "#chan (libera)" {
			t.Fatalf("message = %+v", got.msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the notifier never posted")
	}
}

// TestNoNotifierWithoutAnEndpoint: push is opt-in, and a nil notifier has to be callable or
// every call site needs a nil check and one of them will not have it.
func TestNoNotifierWithoutAnEndpoint(t *testing.T) {
	if n := newNotifier("", ""); n != nil {
		t.Fatalf("a notifier was built with no endpoint: %+v", n)
	}
	var n *notifier
	n.notify(push.Message{Title: "x"}) // must not panic
}

// TestABurstIsOneNotification: a phone that buzzes for every line of a conversation is a phone
// whose notifications get turned off, and then a real mention is missed too.
func TestABurstIsOneNotification(t *testing.T) {
	hits := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hits <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := newNotifier(srv.URL, "")
	n.window = time.Minute
	for i := 0; i < 20; i++ {
		n.notify(mentionMessage("libera", "#chan", "alice", "line"))
	}
	select {
	case <-hits:
	case <-time.After(5 * time.Second):
		t.Fatal("the first notification never arrived")
	}
	// Anything else in flight would arrive within this window; counting on a chan is what
	// makes "exactly one" checkable without racing the handler.
	select {
	case <-hits:
		t.Fatal("a burst of 20 lines produced more than one notification")
	case <-time.After(200 * time.Millisecond):
	}

	// A different window is a different conversation and must get its own.
	n.notify(mentionMessage("libera", "#other", "bob", "line"))
	select {
	case <-hits:
	case <-time.After(5 * time.Second):
		t.Fatal("a second window produced no notification")
	}
}

// TestAFailedSendDoesNotBlockTheNext: a push service having a bad day must not silence the
// next notification.
func TestAFailedSendDoesNotBlockTheNext(t *testing.T) {
	var mu sync.Mutex
	var statuses []int
	done := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		statuses = append(statuses, len(statuses))
		first := len(statuses) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		select {
		case done <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	n := newNotifier(srv.URL, "")
	n.window = 0 // no suppression, so both sends happen
	n.notify(mentionMessage("", "#a", "", "one"))
	n.notify(mentionMessage("", "#b", "", "two"))

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 notifications arrived", i)
		}
	}
}

// TestMentionsNickIsAWholeWord: "alice" in "alicephones" is not a mention, and a notification
// on every line containing a common nick is a notification nobody keeps.
func TestMentionsNickIsAWholeWord(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"salotto: are you there?", true},
		{"hey salotto", true},
		{"salotto", true},
		{"salotto, ping", true},
		{"alicesalotto is not a mention", false},
		{"salottophone", false},
		{"nothing here", false},
		{"SALOTTO", true}, // case-insensitive, as IRC nicks are
		{"@salotto++", true},
	}
	for _, tc := range cases {
		if got := mentionsNick(tc.text, "salotto"); got != tc.want {
			t.Errorf("mentionsNick(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// TestOnMentionNeedsNoClientAttached is the rule the feature exists for: a mention while someone
// is watching is a line on screen, and a notification for it is an interruption.
func TestOnMentionNeedsNoClientAttached(t *testing.T) {
	hits := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hits <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := newNotifier(srv.URL, "")

	// Attached: no notification.
	n.onMention("libera", "#chan", ":alice!a@h PRIVMSG #chan :salotto: hi", "salotto", true)
	select {
	case <-hits:
		t.Fatal("a mention with a client attached produced a notification")
	case <-time.After(250 * time.Millisecond):
	}

	// Detached: one.
	n.onMention("libera", "#chan", ":alice!a@h PRIVMSG #chan :salotto: hi", "salotto", false)
	select {
	case <-hits:
	case <-time.After(5 * time.Second):
		t.Fatal("a mention with nobody attached produced no notification")
	}
}

// TestOnMentionIgnoresOurOwnEcho and lines that do not address us, or the notification is noise.
func TestOnMentionIgnoresOurOwnEchoAndOtherChat(t *testing.T) {
	hits := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hits <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := newNotifier(srv.URL, "")
	n.onMention("libera", "#chan", ":salotto!me@h PRIVMSG #chan :salotto: thinking", "salotto", false)
	n.onMention("libera", "#chan", ":alice!a@h PRIVMSG #chan :something else entirely", "salotto", false)
	n.onMention("libera", "#chan", ":srv 001 salotto :Welcome", "salotto", false)
	select {
	case <-hits:
		t.Fatal("a non-mention produced a notification")
	case <-time.After(250 * time.Millisecond):
	}
}

// TestDispatchNotifiesOnlyWhenNobodyIsAttached is the integration point: the check lives in
// dispatchRouted, and a notification while someone is watching the screen is an interruption.
func TestDispatchNotifiesOnlyWhenNobodyIsAttached(t *testing.T) {
	hits := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hits <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	saved, savedNick := pushNotify, ownNick
	defer func() { pushNotify, ownNick = saved, savedNick }()
	pushNotify, ownNick = newNotifier(srv.URL, ""), "salotto"

	// A server with no clients attached: dispatchRouted's guard is srv.Attached() == 0.
	bouncer := bareServer(t)
	line := []byte(":alice!a@h PRIVMSG #chan :salotto: are you there?")

	dispatchRouted(bouncer, "libera", []string{"#chan"}, line)

	select {
	case <-hits:
	case <-time.After(5 * time.Second):
		t.Fatal("a mention with nobody attached produced no notification")
	}
}

// TestNoNotificationWhileAClientIsAttached is the half the empty-server test cannot reach: with
// zero clients the guard is trivially true, so it proves nothing about the case the feature is
// about. This attaches a real subscriber and asserts the phone stays quiet.
func TestNoNotificationWhileAClientIsAttached(t *testing.T) {
	hits := make(chan struct{}, 4)
	pushes := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hits <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer pushes.Close()

	saved, savedNick := pushNotify, ownNick
	defer func() { pushNotify, ownNick = saved, savedNick }()
	pushNotify, ownNick = newNotifier(pushes.URL, ""), "salotto"

	hub := facade.NewHub(4)
	bouncer := bareServerWith(t, hub)
	sub := facade.NewSubscriber(io.Discard, 4096)
	if !hub.Add(sub) {
		t.Fatal("could not attach a subscriber")
	}
	defer hub.Remove(sub)
	if bouncer.Attached() != 1 {
		t.Fatalf("setup: %d attached", bouncer.Attached())
	}

	dispatchRouted(bouncer, "libera", []string{"#chan"}, []byte(":alice!a@h PRIVMSG #chan :salotto: hi"))
	select {
	case <-hits:
		t.Fatal("a mention with a client attached produced a notification")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestNoNotificationForAStatusLine: the status window is the bouncer talking to itself, and a
// mention there is not a mention.
func TestNoNotificationForAStatusLine(t *testing.T) {
	hits := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hits <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	saved, savedNick := pushNotify, ownNick
	defer func() { pushNotify, ownNick = saved, savedNick }()
	pushNotify, ownNick = newNotifier(srv.URL, ""), "salotto"

	bouncer := bareServer(t)
	// A line that would otherwise qualify: a real nick, a real whole-word mention, and the
	// only thing wrong with it is the window. Written this way because the first version of
	// this test used a server NOTICE, which parseMention already rejects - so it passed for a
	// reason that had nothing to do with the window check.
	dispatchRouted(bouncer, "libera", []string{"status"}, []byte(":alice!a@h PRIVMSG status :salotto: hi"))

	select {
	case <-hits:
		t.Fatal("a status line produced a notification")
	case <-time.After(250 * time.Millisecond):
	}
}

// bareServer is a facade with no clients and no networks: enough for dispatchRouted, which
// only needs somewhere to broadcast to.
func bareServer(t *testing.T) *facade.Server {
	t.Helper()
	return bareServerWith(t, facade.NewHub(4))
}

// bareServerWith is bareServer with the hub supplied, for a test that needs to attach.
func bareServerWith(t *testing.T, hub *facade.Hub) *facade.Server {
	t.Helper()
	srv, err := facade.New(facade.Config{
		ServerName: "salotto",
		Hub:        hub,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// TestTheNotifierReportsWhatHappened is the observability half, and it was missing: a send
// endpoint that answers 403 and a mention that never fires both look exactly like "the phone is
// quiet". Tried against the deployment, there was nothing anywhere to distinguish them.
func TestTheNotifierReportsWhatHappened(t *testing.T) {
	var mu sync.Mutex
	var notes []string
	record := func(msg string, args ...any) {
		// The args are the whole point: "refused" without "403" is a line that cannot
		// distinguish a bad token from a front end with push turned off.
		mu.Lock()
		notes = append(notes, msg+" "+fmt.Sprint(args...))
		mu.Unlock()
	}

	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer refuse.Close()

	n := newNotifier(refuse.URL, "wrong-token")
	n.log = record
	n.onMention("libera", "#chan", ":alice!a@h PRIVMSG #chan :salotto: hi", "salotto", false)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := strings.Join(notes, "|")
		mu.Unlock()
		if strings.Contains(got, "refused") {
			if !strings.Contains(got, "403") {
				t.Fatalf("the refusal was reported without the status, so an operator cannot tell a bad token from an unconfigured front end: %q", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("a refused notification said nothing: %v", notes)
}

// TestASuccessfulSendIsAlsoReported: the success path needs a line too, because "no failure
// logged" and "the feature is working" have to be distinguishable from a log.
func TestASuccessfulSendIsAlsoReported(t *testing.T) {
	var mu sync.Mutex
	var notes []string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()

	n := newNotifier(ok.URL, "tok")
	n.log = func(msg string, args ...any) {
		mu.Lock()
		notes = append(notes, msg+" "+fmt.Sprint(args...))
		mu.Unlock()
	}
	n.onMention("libera", "#chan", ":alice!a@h PRIVMSG #chan :salotto: hi", "salotto", false)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := strings.Join(notes, "|")
		mu.Unlock()
		if strings.Contains(got, "sent") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("a successful send said nothing: %v", notes)
}
