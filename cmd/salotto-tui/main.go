// salotto-tui is the terminal client.
//
// The web front end runs this on a PTY for each browser session, which is what makes the
// browser a terminal rather than an imitation of one: this program does not know the
// difference, and neither does the renderer. It is also the binary a person runs in a
// real terminal, so there is exactly one interactive client and no way for the browser
// to fall behind it.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/iakat/salotto/internal/ui"
)

var versionString = "dev"

func main() {
	bouncer := flag.String("bouncer", "", "bouncer address, host:port (empty for a local UI)")
	nick := flag.String("nick", "", "nick to register with the bouncer")
	window := flag.String("window", "", "initial window (default: the nick's own window, or status)")
	settingsPath := flag.String("settings", "", "path to the client settings file (empty for defaults, or $SALOTTO_SETTINGS)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("salotto-tui %s\n", versionString)
		return
	}

	// On a PTY the colour profile resolves itself. Under the web front end the TERM the
	// bridge sets is xterm-256color, which is what lipgloss needs to emit the 8-bit SGR
	// the emulator understands.
	ui.Configure()

	var tr *ui.Transport
	if *bouncer != "" {
		var err error
		tr, err = ui.Dial(*bouncer)
		if err != nil {
			slog.Error("bouncer dial failed", "err", err)
			os.Exit(1)
		}
		defer tr.Close()
		// Register, or the bouncer holds a connection that never becomes a user: no
		// window list, no nicklists, no history, and the terminal sits on an empty status
		// window looking like it works. The old web front end did this in its own dial;
		// moving the dial into the child meant moving this too, and forgetting it was
		// invisible until someone opened the page.
		// A bouncer without a nick is not a session: it would register with an empty
		// NICK, be rejected, and show a blank terminal. Refuse instead of pretending.
		if *nick == "" {
			slog.Error("a bouncer address needs -nick: a client of a bouncer is that user")
			os.Exit(1)
		}
		tr.Register(*nick, "")
	}

	// The starting window is the user's own status window, which is where a bouncer
	// tells you what it did while you were away.
	active := *window
	if active == "" {
		if *nick != "" {
			active = *nick
		} else {
			active = "status"
		}
	}

	// Settings are the client's, so each front end loads its own file. $SALOTTO_SETTINGS
	// lets the web bridge point every browser session at the same one without adding a flag
	// to the command line it builds.
	settings := *settingsPath
	if settings == "" {
		settings = os.Getenv("SALOTTO_SETTINGS")
	}
	prog := ui.NewProgram(active, tr).WithSettings(settings)
	if tr != nil {
		// The transport is pumped into the model from one goroutine, and bubbletea's
		// update loop is what renders it: two writers to the model would interleave.
		go pump(prog, tr)
	}

	p := ui.TUIProgram(prog, os.Stdin, os.Stdout)
	if _, err := p.Run(); err != nil {
		slog.Error("tui exited", "err", err)
		os.Exit(1)
	}
}

// pump feeds the transport into the Program. It is the only goroutine that touches the
// model from the transport side, which is why the Program's lock exists.
func pump(prog *ui.Program, tr *ui.Transport) {
	for {
		msg, ok := prog.Next()
		if !ok {
			return
		}
		prog.Step(msg)
	}
}
