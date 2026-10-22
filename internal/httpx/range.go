package httpx

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ByteRange is one satisfiable range of a body, both ends inclusive.
type ByteRange struct{ Start, End int64 }

// ErrBadRange covers a malformed header and a range that cannot be satisfied: both are 416.
var ErrBadRange = errors.New("range not satisfiable")

// ParseRange parses a Range header against a body of size bytes (RFC 9110 §14).
// It returns (nil, nil) when the whole body should be sent with 200: no header, or more than
// one range — multi-range is legal to ignore, and no player asks for it (plan §12 #10).
//
// It follows http.ServeContent except where the RFC is stricter: "bytes=-0" and a signed
// position ("+1") are 416, where ServeContent accepts them.
func ParseRange(header string, size int64) (*ByteRange, error) {
	if header == "" {
		return nil, nil
	}
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return nil, ErrBadRange
	}
	if strings.Contains(spec, ",") {
		return nil, nil
	}
	first, last, ok := strings.Cut(strings.Trim(spec, " \t"), "-")
	if !ok {
		return nil, ErrBadRange
	}

	if first == "" { // suffix: the last n bytes
		n, err := digits(last)
		if err != nil || n == 0 || size == 0 {
			return nil, ErrBadRange
		}
		return &ByteRange{max(size-n, 0), size - 1}, nil
	}

	start, err := digits(first)
	if err != nil || start >= size {
		return nil, ErrBadRange
	}
	if last == "" {
		return &ByteRange{start, size - 1}, nil
	}
	end, err := digits(last)
	if err != nil || end < start {
		return nil, ErrBadRange
	}
	return &ByteRange{start, min(end, size-1)}, nil
}

// digits parses 1*DIGIT. strconv alone would also accept a leading '+'.
func digits(s string) (int64, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, ErrBadRange
	}
	return strconv.ParseInt(s, 10, 64) // overflow → error
}

func (b ByteRange) Len() int64 { return b.End - b.Start + 1 }

// ContentRange is the 206 response header value, e.g. "bytes 100-199/10000".
func (b ByteRange) ContentRange(size int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", b.Start, b.End, size)
}
