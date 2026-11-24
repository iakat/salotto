# highlights.star - count lines that mention salotto.
#
# Drop this in script_dir and it is live within a couple of seconds. Edit it and the next
# version is live just as fast: the daemon re-stats the directory on a timer and reloads
# whatever changed. Nothing here can reach the network, the file system or the clock - the
# interpreter has no such operations.

def on_line(network, line):
    if "PRIVMSG" not in line:
        return
    if "salotto" not in line:
        return
    state["mentions"] = state.get("mentions", 0) + 1
    state["last_mention"] = line
    log(network, "mention #%d" % state["mentions"])
