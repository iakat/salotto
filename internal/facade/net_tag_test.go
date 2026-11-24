package facade

import (
	"strings"
	"testing"

	"github.com/iakat/salotto/internal/ircline"
)

// The network tag is added to every line the bouncer sends, live or replayed, and exactly once.
//
// Two copies of one tag is not a display quirk: a client reads whichever the parser found first,
// so a second copy that disagreed (a relayed line from another bouncer) would be silently
// ignored or silently preferred, and nobody would know which. A vendor tag is ours, so a line
// that already carries one knows better than we do.
func TestTheNetworkTagIsAddedOnce(t *testing.T) {
	const ts = int64(1700000000000000000)

	live := appendLiveLine(nil, []byte(":n!u@h PRIVMSG #c :hi"), "libera", ts)
	if got := strings.Count(string(live), "salotto.network=libera"); got != 1 {
		t.Fatalf("a live line carries the network tag %d times: %q", got, live)
	}

	// A line that came with the tag already on it - a bouncer relaying a bouncer - keeps its own
	// value and gains no second copy.
	relayed := appendLiveLine(nil, []byte("@salotto.network=other PRIVMSG #c :hi"), "libera", ts)
	if strings.Count(string(relayed), "salotto.network=") != 1 {
		t.Fatalf("a relayed live line grew a second network tag: %q", relayed)
	}
	if !strings.Contains(string(relayed), "salotto.network=other") {
		t.Fatalf("a relayed live line lost its own network: %q", relayed)
	}

	replay := appendReplayLine(nil, []byte(":n!u@h PRIVMSG #c :hi"), "libera-#c-replay", "libera", ts)
	if got := strings.Count(string(replay), "salotto.network=libera"); got != 1 {
		t.Fatalf("a replayed line carries the network tag %d times: %q", got, replay)
	}
	// The batch tag and the time tag are still there: the network is added to the line, it does
	// not replace what a client already needs.
	for _, want := range []string{"batch=libera-#c-replay", "time="} {
		if !strings.Contains(string(replay), want) {
			t.Fatalf("a replayed line lost %q: %q", want, replay)
		}
	}

	// No network (a ring that was not told one) means no tag at all, rather than an empty one: a
	// client would group those lines under a heading named "".
	bare := appendReplayLine(nil, []byte(":n!u@h PRIVMSG #c :hi"), "x-#c-replay", "", ts)
	if strings.Contains(string(bare), "salotto.network") {
		t.Fatalf("a line with no network got a network tag: %q", bare)
	}
}

// TestANetworkNameWithASpaceSurvivesTheWire: the network name comes from the operator's config
// file, where "my net" is a perfectly ordinary name. An unescaped space ends the tag blob, so the
// line would arrive with its verb inside the name - the message disappears and something that is
// not a message takes its place. The test reads the tag back the way a client does.
func TestANetworkNameWithASpaceSurvivesTheWire(t *testing.T) {
	// The batch id is left space-free on purpose: it is derived from the network name by
	// string concatenation (a separate, pre-existing limitation of the BATCH declaration), and
	// what this test is about is the tag this change writes.
	line := appendReplayLine(nil, []byte(":n!u@h PRIVMSG #c :hi"), "batchid", "my net", 1)
	if !strings.Contains(string(line), `salotto.network=my\snet`) {
		t.Fatalf("the network name was written unescaped: %q", line)
	}

	tags, rest := ircline.Tags(line)
	_, rest = ircline.Prefix(rest)
	if len(rest) == 0 || !ircline.IsVerb(rest, "PRIVMSG") {
		t.Fatalf("the tag blob ended inside the value, so the line is not a PRIVMSG: %q", line)
	}
	if got := ircline.TagText(ircline.TagValue(tags, []byte(ircline.TagNetwork))); got != "my net" {
		t.Fatalf("the network came back as %q, want %q", got, "my net")
	}
}

// TestAnEmptyNetworkTagIsNotWritten is the same rule from the other end: a line whose tag is
// present but empty is the fallback case, so it must be treated as "no network" rather than as a
// network called "".
func TestAnEmptyNetworkTagIsNotWritten(t *testing.T) {
	got := appendLiveLine(nil, []byte("PRIVMSG #c :hi"), "", 1)
	if strings.Contains(string(got), "salotto.network") {
		t.Fatalf("an empty network was written as a tag: %q", got)
	}
}
