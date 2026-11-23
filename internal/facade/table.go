package facade

import "strings"

// table renders ZNC's grid table, which is what every list command answers
// with: STATUS, LISTCHANS, LISTNETWORKS, LISTCLIENTS, LISTSERVERS and TRAFFIC
// all build a CTable and hand it to PutStatus(CTable), which emits one STATUS
// line per row (Client.cpp:718).
//
// The exact shape matters because the whole point of these commands is that a
// ZNC user's client and muscle memory still work. From CTable::GetLine
// (Utils.cpp:844-910) the layout is, for a table with headers H and n rows:
//
//	+----+-----+     index 0
//	| H1 | H2  |     index 1
//	+----+-----+     index 2
//	| r1 | ... |     index 3 .. n+2
//	+----+-----+     index n+3
//
// A column is as wide as its header or its widest cell, and cells are padded on
// the right. The separator is built as "+-" then, per column, width hyphens
// followed by "-+-" between columns and "-+" at the end, which is why the line
// starts "+-" and every column contributes width+1.
type table struct {
	headers []string
	rows    [][]string
	widths  []int
}

func newTable(headers ...string) *table {
	t := &table{headers: headers, widths: make([]int, len(headers))}
	for i, h := range headers {
		t.widths[i] = len(h)
	}
	return t
}

// add appends a row. Missing cells are treated as empty, and a row longer than
// the header is truncated rather than corrupting the layout: ZNC's AddRow sizes
// the row from the headers, so an extra cell simply has nowhere to go.
func (t *table) add(cells ...string) {
	row := make([]string, len(t.headers))
	for i := range row {
		if i < len(cells) {
			row[i] = cells[i]
			if w := len(cells[i]); w > t.widths[i] {
				t.widths[i] = w
			}
		}
	}
	t.rows = append(t.rows, row)
}

// len returns the number of data rows, which is what the list commands test to
// decide whether they also print a "Total:" line.
func (t *table) len() int { return len(t.rows) }

// lines renders the table the way GetLine would, one string per line.
func (t *table) lines() []string {
	if len(t.headers) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.rows)+4)
	out = append(out, t.separator())
	out = append(out, t.render(t.headers))
	out = append(out, t.separator())
	for _, row := range t.rows {
		out = append(out, t.render(row))
	}
	out = append(out, t.separator())
	return out
}

func (t *table) separator() string {
	var b strings.Builder
	b.WriteString("+-")
	for i := range t.headers {
		b.WriteString(strings.Repeat("-", t.widths[i]))
		if i == len(t.headers)-1 {
			b.WriteString("-+")
		} else {
			b.WriteString("-+-")
		}
	}
	return b.String()
}

func (t *table) render(cells []string) string {
	var b strings.Builder
	b.WriteString("| ")
	for i := range t.headers {
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		b.WriteString(cell)
		if pad := t.widths[i] - len(cell); pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		if i == len(t.headers)-1 {
			b.WriteString(" |")
		} else {
			b.WriteString(" | ")
		}
	}
	return b.String()
}

// byteStr formats a byte count the way ZNC's CString::ToByteStr does
// (ZNCString.cpp:1246): whole units only, strict greater-than so exactly 1024
// bytes still reads "1024 B", and a trailing unit.
func byteStr(n uint64) string {
	const (
		kib = 1024
		mib = kib * 1024
		gib = mib * 1024
		tib = gib * 1024
	)
	switch {
	case n > tib:
		return itoa(n/tib) + " TiB"
	case n > gib:
		return itoa(n/gib) + " GiB"
	case n > mib:
		return itoa(n/mib) + " MiB"
	case n > kib:
		return itoa(n/kib) + " KiB"
	default:
		return itoa(n) + " B"
	}
}

// itoa avoids strconv's allocation in the render path, which runs while the
// read goroutine is holding nothing interesting but the client is waiting.
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
