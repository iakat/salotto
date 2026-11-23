package facade

import (
	"bytes"
	"strings"
	"testing"
)

// containsLine reports whether any line contains needle.
func containsLine(lines []string, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// TestEchoMessageGate pins the rule ZNC applies in CClient::EchoMessage
// (Client.cpp:1138): a client sees its own line back only when it negotiated
// echo-message. Without the gate, a client that draws its own messages locally
// shows every line twice — which is exactly the difference the e2e harness found
// against ZNC 1.10.2.
func TestEchoMessageGate(t *testing.T) {
	s, _, sender, senderOut := smallRings(t)

	var otherOut bytes.Buffer
	other := NewSubscriber(&otherOut, 64*1024)
	t.Cleanup(other.Close)
	s.hub.Add(other)
	defer s.hub.Remove(other)

	s.AttachWindow(sender, "libera", "#c")
	s.AttachWindow(other, "libera", "#c")

	line := []byte(":me!u@h PRIVMSG #c :hi")

	// No echo-message: the author must not see it, the other client must.
	s.BroadcastWindowsFrom("libera", []string{"#c"}, 0, line, 1, sender)
	otherLines := flushLines(t, other, &otherOut)
	authorLines := flushLines(t, sender, senderOut)
	if containsLine(authorLines, " :hi") {
		t.Fatalf("author saw its own line without echo-message: %q", authorLines)
	}
	if !containsLine(otherLines, " :hi") {
		t.Fatalf("the other client did not see the line: %q", otherLines)
	}

	// With echo-message: the author must see it too.
	sender.SetEchoMessage(true)
	s.BroadcastWindowsFrom("libera", []string{"#c"}, 0, line, 2, sender)
	authorLines = flushLines(t, sender, senderOut)
	if !containsLine(authorLines, " :hi") {
		t.Fatalf("author did not see its own line with echo-message: %q", authorLines)
	}
}
