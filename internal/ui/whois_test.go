package ui

import (
	"strings"
	"testing"
)

// A whois answer arrives as up to eight numerics and is one answer. Drawn raw it is a protocol
// dump:
//
//	311 nicholas alice ~a h * :Alice Smith
//	319 nicholas alice :#salotto #dev
//	312 nicholas alice irc.libera.chat :Libera Chat
//	318 nicholas alice :End of /WHOIS list
//
// The numbers are the server's names for fields, the colons are separators the wire needs and the
// reader does not, and the last line is bookkeeping. What was asked was "who is alice".

// whoisReply feeds a complete whois exchange through the transport and returns what came out.
func whoisReply(t *testing.T, lines ...string) []Line {
	t.Helper()
	tr := newTestTransport(t)
	for _, l := range lines {
		tr.handle([]byte(l))
	}
	var out []Line
	for {
		line, ok := nextIncoming(t, tr)
		if !ok {
			return out
		}
		out = append(out, Line(line))
	}
}

// TestAWhoisIsOneAnswerNotEightNumerics is the defect: the block drawn is the answer, and no line
// of it is a raw numeric.
func TestAWhoisIsOneAnswerNotEightNumerics(t *testing.T) {
	lines := whoisReply(t,
		":srv 311 nicholas alice ~a host.example * :Alice Smith\r\n",
		":srv 319 nicholas alice :#salotto #dev\r\n",
		":srv 312 nicholas alice irc.libera.chat :Libera Chat\r\n",
		":srv 318 nicholas alice :End of /WHOIS list\r\n",
	)

	if len(lines) != 4 {
		t.Fatalf("the answer is %d lines, want 4 (header + 3 fields):\n%s", len(lines), dumpLines(lines))
	}
	if lines[0].Text != "alice" {
		t.Fatalf("the block is headed %q, want the nick", lines[0].Text)
	}
	for _, l := range lines {
		if strings.Contains(l.Text, "318") || strings.Contains(l.Text, "311") ||
			strings.Contains(l.Text, "319") || strings.Contains(l.Text, "312") {
			t.Fatalf("a raw numeric survived into the answer: %q", l.Text)
		}
		if strings.Contains(l.Text, "End of /WHOIS") {
			t.Fatalf("the reply's bookkeeping was drawn: %q", l.Text)
		}
	}
}

// TestTheFieldsSayWhatTheyMean pins each numeric's words, because the whole point is that the user
// reads "logged in as" rather than "330".
func TestTheFieldsSayWhatTheyMean(t *testing.T) {
	lines := whoisReply(t,
		":srv 311 nicholas alice ~a host.example * :Alice Smith\r\n",
		":srv 319 nicholas alice :#salotto #dev\r\n",
		":srv 330 nicholas alice alicesmith :is logged in as\r\n",
		":srv 671 nicholas alice :is using a secure connection\r\n",
		":srv 318 nicholas alice :End\r\n",
	)
	got := strings.Join(texts(lines), "\n")

	for _, want := range []string{
		"~a@host.example (Alice Smith)",
		"on #salotto #dev",
		"logged in as alicesmith",
		"is using a secure connection",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the answer does not say %q:\n%s", want, got)
		}
	}
}

// TestAnIdleTimeIsNotInSeconds: "7200s" makes the reader do arithmetic the client can do.
func TestAnIdleTimeIsNotInSeconds(t *testing.T) {
	lines := whoisReply(t,
		":srv 311 nicholas alice ~a h * :Alice\r\n",
		":srv 317 nicholas alice 7300 1700000000 :seconds idle, signon time\r\n",
		":srv 318 nicholas alice :End\r\n",
	)
	got := strings.Join(texts(lines), "\n")
	if !strings.Contains(got, "idle 2h1m") {
		t.Fatalf("the idle time is not readable:\n%s", got)
	}
	if strings.Contains(got, "7300") {
		t.Fatalf("the raw seconds survived:\n%s", got)
	}
}

// TestTheAnswerGoesWhereTheQuestionWasAsked: typed in a channel, the answer belongs in that
// channel. Landing in status is an answer the user will not read, and "/whois did nothing" is the
// same complaint the /msg fix was about.
func TestTheAnswerGoesWhereTheQuestionWasAsked(t *testing.T) {
	tr := newTestTransport(t)
	tr.SetActive("#salotto")
	// The command goes out through Command, which is what records where the question was asked.
	if got := tr.Command("/whois alice"); got != "WHOIS alice" {
		t.Fatalf("the command went out as %q", got)
	}
	tr.handle([]byte(":srv 311 nicholas alice ~a h * :Alice\r\n"))
	tr.handle([]byte(":srv 318 nicholas alice :End\r\n"))

	line, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("the answer drew nothing")
	}
	if winName(line.Window) != "#salotto" {
		t.Fatalf("the answer landed in %q; it was asked for from #salotto", line.Window)
	}
}

// TestAnUnaskedWhoisStillDraws is the negative control for the window: a reply to a question this
// client did not ask has no window that asked, and status is the honest default.
func TestAnUnaskedWhoisStillDraws(t *testing.T) {
	tr := newTestTransport(t)
	// The user is looking at a channel, and the reply answers a question this client never
	// asked - a replayed one, or a script asking on our behalf. There is no window that asked,
	// so status is the honest home: dropping it into whatever the user happens to be reading
	// would put a stranger's answer in the middle of a conversation it has nothing to do with.
	tr.SetActive("#salotto")
	tr.handle([]byte(":srv 311 nicholas alice ~a h * :Alice\r\n"))
	tr.handle([]byte(":srv 318 nicholas alice :End\r\n"))

	line, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("a whois reply nobody asked for drew nothing")
	}
	if line.Window != "status" {
		t.Fatalf("the answer landed in %q, want status", line.Window)
	}
}

// TestAWhoisEmptyOfInformationSaysSo: a whois that produced only its own end is not a heading over
// nothing.
func TestAWhoisWithNoInformationSaysSo(t *testing.T) {
	lines := whoisReply(t, ":srv 318 nicholas nobody :End of /WHOIS list\r\n")
	if len(lines) != 1 {
		t.Fatalf("the answer is %d lines: %s", len(lines), dumpLines(lines))
	}
	if !strings.Contains(lines[0].Text, "no information") {
		t.Fatalf("the answer is %q", lines[0].Text)
	}
}

// TestATwoWhoisInterleaveDoesNotMixThemUp: two questions, answers arriving interleaved, each block
// keeps its own fields.
func TestATwoWhoisInterleaveDoesNotMixThemUp(t *testing.T) {
	tr := newTestTransport(t)
	tr.SetActive("#salotto")
	tr.Command("/whois alice")
	tr.Command("/whois bob")
	tr.handle([]byte(":srv 311 nicholas alice ~a h * :Alice\r\n"))
	tr.handle([]byte(":srv 311 nicholas bob ~b h * :Bob\r\n"))
	tr.handle([]byte(":srv 318 nicholas alice :End\r\n"))
	tr.handle([]byte(":srv 318 nicholas bob :End\r\n"))

	var out []Line
	for {
		line, ok := nextIncoming(t, tr)
		if !ok {
			break
		}
		out = append(out, Line(line))
	}
	got := strings.Join(texts(out), "\n")
	if !strings.Contains(got, "~a@h (Alice)") || !strings.Contains(got, "~b@h (Bob)") {
		t.Fatalf("the two answers are not both present and distinct:\n%s", got)
	}
}

// TestAnUnknownNumericIsStillDrawnRaw is the negative control for the family: a numeric this file
// does not understand must reach the user, not be swallowed. A raw line is worse than a nice one
// and better than silence.
func TestAnUnknownNumericIsStillDrawnRaw(t *testing.T) {
	lines := whoisReply(t, ":srv 275 nicholas alice :is connected via TLS\r\n")
	if len(lines) != 1 {
		t.Fatalf("the numeric drew %d lines", len(lines))
	}
	if !strings.Contains(lines[0].Text, "275") {
		t.Fatalf("an unknown numeric was swallowed or rewritten: %q", lines[0].Text)
	}
}

// TestAChannelWhoisIsNotAQuery pins that the whois state does not invent a window for the subject:
// asking about alice must not open alice's query.
func TestAChannelWhoisIsNotAQuery(t *testing.T) {
	tr := newTestTransport(t)
	tr.SetActive("#salotto")
	tr.Command("/whois alice")
	tr.handle([]byte(":srv 311 nicholas alice ~a h * :Alice\r\n"))
	tr.handle([]byte(":srv 318 nicholas alice :End\r\n"))

	line, _ := nextIncoming(t, tr)
	if winName(line.Window) == "alice" {
		t.Fatal("the whois opened a query window named after its subject")
	}
}

func texts(lines []Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

func dumpLines(lines []Line) string {
	return strings.Join(texts(lines), "\n")
}
