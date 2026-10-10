package cli

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"
)

// table is a drop-in for text/tabwriter (cells end in '\t', two spaces of
// padding) that measures cells by display width: tabwriter counts runes, so
// a column holding Chinese provider or project names came out misaligned.
// A line without a tab ends the current block of aligned lines, as in
// tabwriter.
type table struct {
	out io.Writer
	buf bytes.Buffer
}

func newTable(out io.Writer) *table { return &table{out: out} }

func (t *table) Write(p []byte) (int, error) { return t.buf.Write(p) }

func (t *table) Flush() error {
	text := t.buf.String()
	t.buf.Reset()
	lines := strings.SplitAfter(text, "\n")
	var b strings.Builder
	for start := 0; start < len(lines); {
		if !strings.Contains(lines[start], "\t") {
			b.WriteString(lines[start])
			start++
			continue
		}
		end := start
		for end < len(lines) && strings.Contains(lines[end], "\t") {
			end++
		}
		var widths []int
		cells := make([][]string, 0, end-start)
		for _, line := range lines[start:end] {
			row := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
			cells = append(cells, row)
			for i, c := range row[:len(row)-1] {
				if i >= len(widths) {
					widths = append(widths, 0)
				}
				widths[i] = max(widths[i], displayWidth(c))
			}
		}
		for k, row := range cells {
			var l strings.Builder
			for i, c := range row {
				l.WriteString(c)
				if i < len(row)-1 {
					l.WriteString(strings.Repeat(" ", widths[i]-displayWidth(c)+2))
				}
			}
			b.WriteString(strings.TrimRight(l.String(), " "))
			if strings.HasSuffix(lines[start+k], "\n") {
				b.WriteByte('\n')
			}
		}
		start = end
	}
	_, err := io.WriteString(t.out, b.String())
	return err
}

// displayWidth counts the terminal columns of s: East Asian wide and
// fullwidth characters, and emoji, take two.
func displayWidth(s string) int {
	n := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch {
		case r == utf8.RuneError && size <= 1:
			n++
		case r < 0x1100:
			n++
		case r <= 0x115f, // Hangul Jamo
			r >= 0x2e80 && r <= 0xa4cf && r != 0x303f, // CJK … Yi
			r >= 0xac00 && r <= 0xd7a3,                // Hangul syllables
			r >= 0xf900 && r <= 0xfaff,                // CJK compatibility ideographs
			r >= 0xfe30 && r <= 0xfe4f,                // CJK compatibility forms
			r >= 0xff00 && r <= 0xff60,                // fullwidth forms
			r >= 0xffe0 && r <= 0xffe6,
			r >= 0x1f300 && r <= 0x1faff, // emoji
			r >= 0x20000 && r <= 0x3fffd:
			n += 2
		default:
			n++
		}
	}
	return n
}
