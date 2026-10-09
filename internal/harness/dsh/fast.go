package dsh

import (
	"bytes"
	"strconv"
	"time"
)

// skippable recognizes the compact envelope written by DSH. Unknown layouts
// take the normal decoder path. Only the activity time of these records matters.
func skippable(line []byte) (time.Time, bool) {
	const prefix = `{"type":"`
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return time.Time{}, false
	}
	rest := line[len(prefix):]
	n := bytes.IndexByte(rest, '"')
	if n < 0 || bytes.IndexByte(rest[:n], '\\') >= 0 {
		return time.Time{}, false
	}
	switch string(rest[:n]) {
	case "session", "session/title", "user/message", "assistant/message", "compaction/summary":
		return time.Time{}, false
	}
	rest = rest[n+1:]
	const seq = `,"seq":`
	if !bytes.HasPrefix(rest, []byte(seq)) {
		return time.Time{}, false
	}
	rest = rest[len(seq):]
	n = 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if n == 0 {
		return time.Time{}, false
	}
	rest = rest[n:]
	const tm = `,"time":`
	if !bytes.HasPrefix(rest, []byte(tm)) {
		return time.Time{}, false
	}
	rest = rest[len(tm):]
	n = bytes.IndexByte(rest, ',')
	if n < 0 || !bytes.HasPrefix(rest[n:], []byte(`,"data":`)) {
		return time.Time{}, false
	}
	millis, err := strconv.ParseInt(string(rest[:n]), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return msTime(num(millis)), true
}
