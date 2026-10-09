package clinetask

import (
	"encoding/json"
	"math"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// num decodes a JSON number that may arrive as an integer, a float, a quoted
// string or null. Cline and Roo always write plain numbers, but older
// revisions and some forks quote them, and a null means "absent".
type num int64

func (n *num) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
		if s == "" {
			*n = 0
			return nil
		}
	}
	// A value that is not a number at all degrades to zero rather than failing
	// the whole document: one malformed field must not cost us every request in
	// the file.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*n = 0
		return nil
	}
	*n = num(int64(f))
	return nil
}

// fnum decodes a JSON float that may arrive as a number, a quoted string or
// null. NaN is not representable in JSON (JSON.stringify turns it into null),
// so a null simply means "no cost recorded".
type fnum float64

func (f *fnum) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
		if s == "" {
			*f = 0
			return nil
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		// strconv.ParseFloat happily accepts "NaN" and "Inf", which carry no
		// cost information and would poison every total they are added to.
		*f = 0
		return nil
	}
	*f = fnum(v)
	return nil
}

// tsVal decodes a Cline message timestamp. The extensions write epoch
// milliseconds as a JSON number; some fixtures and forks write RFC3339 strings
// or quoted epochs, and a bare number below the millisecond threshold is read
// as epoch seconds.
type tsVal time.Time

func (t *tsVal) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*t = tsVal(time.Time{})
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		str = strings.TrimSpace(str)
		if str == "" {
			*t = tsVal(time.Time{})
			return nil
		}
		if ms, err := strconv.ParseInt(str, 10, 64); err == nil {
			*t = tsVal(epochToTime(ms))
			return nil
		}
		if parsed, err := time.Parse(time.RFC3339Nano, str); err == nil {
			*t = tsVal(parsed)
			return nil
		}
		// An unparseable timestamp degrades to the zero time, which the callers
		// treat as "skip this entry". Returning an error here would abort the
		// decode of the whole array and lose every other request with it.
		*t = tsVal(time.Time{})
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		*t = tsVal(time.Time{})
		return nil
	}
	if f >= 1e11 {
		// Epoch milliseconds, possibly with a fractional part. The fraction is
		// scaled separately because float64 cannot hold both the millisecond
		// count and the sub-millisecond digits at full precision.
		ms := int64(f)
		ns := int64(math.Round((f - float64(ms)) * float64(time.Millisecond)))
		*t = tsVal(time.UnixMilli(ms).Add(time.Duration(ns)).UTC())
		return nil
	}
	*t = tsVal(epochToTime(int64(f)))
	return nil
}

// epochToTime converts an epoch value to a time, treating anything below the
// millisecond threshold as seconds.
func epochToTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	if v < 1e11 {
		return time.Unix(v, 0).UTC()
	}
	return time.UnixMilli(v).UTC()
}

func (t tsVal) Time() time.Time { return time.Time(t) }

// runtimeGOOS is a seam so the platform-specific root lists can be exercised
// from tests on any host. Tests override it and restore it with a defer.
var runtimeGOOS = func() string { return runtime.GOOS }
