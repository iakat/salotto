package ui

import (
	"testing"
	"time"
)

// The model cannot tell a replayed line from a live one by itself: the difference is a tag the
// bouncer puts on the wire, and the transport is the only place that reads it. These tests are at
// the transport, because the mutations that would mark every line as a replay - or none - live
// there and a model-level test feeds whatever Line it likes.

// TestOnlyABatchedLineIsMarkedAsAReplay.
//
// This is the negative control for the whole feature. If the transport marked an untagged line as
// a replay, the model would compare live traffic against the window and drop any message that
// repeated an earlier one - silent loss, on a server that never claimed to be replaying anything.
func TestOnlyABatchedLineIsMarkedAsAReplay(t *testing.T) {
	cases := []struct {
		name string
		wire string
		want bool
	}{
		{
			name: "a live line",
			wire: ":alice!a@h PRIVMSG #salotto :hello",
			want: false,
		},
		{
			name: "a live line with other tags",
			wire: "@time=2026-11-24T10:00:00.000Z;salotto.network=libera :alice!a@h PRIVMSG #salotto :hello",
			want: false,
		},
		{
			name: "a replayed line",
			wire: "@batch=libera-#salotto-replay;time=2026-11-24T10:00:00.000Z :alice!a@h PRIVMSG #salotto :hello",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTransport(t)
			tr.handle([]byte(tc.wire + "\r\n"))
			line, ok := nextIncoming(t, tr)
			if !ok {
				t.Fatal("nothing came out of the transport")
			}
			if line.Replayed != tc.want {
				t.Fatalf("Replayed = %v for %q, want %v", line.Replayed, tc.wire, tc.want)
			}
			if tc.want && line.Batch == "" {
				t.Fatal("a replayed line carried no batch id")
			}
			if !tc.want && line.Batch != "" {
				t.Fatalf("a live line carried batch id %q", line.Batch)
			}
		})
	}
}

// TestABatchIdReachesTheModel.
//
// The id is what lets two separate replays of the same history both be reconciled: without it the
// second replay would read the first one's counts and draw one extra copy of everything it has.
// A transport that dropped the id would make the model's per-batch bookkeeping meaningless, and
// no model-level test could see it.
func TestABatchIdReachesTheModel(t *testing.T) {
	tr := newTestTransport(t)
	tr.handle([]byte("@batch=libera-#salotto-replay :alice!a@h PRIVMSG #salotto :hello\r\n"))
	line, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("nothing came out of the transport")
	}
	if line.Batch != "libera-#salotto-replay" {
		t.Fatalf("batch = %q, want the id the bouncer sent", line.Batch)
	}
}

// TestTwoBatchesOfTheSameLineAreBothKeptInTheModel is the model's half of the same rule, at the
// granularity a user sees: re-attach, replay, re-attach again, replay again. The client held the
// line once and must still hold it once.
func TestTwoBatchesOfTheSameLineAreBothKeptInTheModel(t *testing.T) {
	m := New("status")
	m.connected = true
	at := time.Now()
	wire := func(batch string) string {
		return "@batch=" + batch + " :alice!a@h PRIVMSG #salotto :hello"
	}
	m = m.applyWireAt(t, wire("b1"), at)
	m = m.applyWireAt(t, wire("b2"), at)
	if got := lineCount(m, "#salotto"); got != 1 {
		t.Fatalf("the window holds %d line(s) after two batches of the same line, want 1", got)
	}
	// And a genuinely repeated line inside one batch is drawn: alice said it twice.
	m = m.applyWireAt(t, wire("b3"), at)
	m = m.applyWireAt(t, wire("b3"), at)
	if got := lineCount(m, "#salotto"); got != 2 {
		t.Fatalf("the window holds %d line(s) after one batch carrying two copies, want 2", got)
	}
}

// applyWireAt is applyWire with a fixed clock, so a test can reason about lines without the wall
// clock entering into it.
func (m Model) applyWireAt(t *testing.T, wire string, _ time.Time) Model {
	t.Helper()
	return m.applyWire(t, wire)
}
