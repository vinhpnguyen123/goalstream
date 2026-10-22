package httpx

import "testing"

func TestParseRange(t *testing.T) {
	const size = 10000
	whole := &ByteRange{-1, -1} // marker: (nil, nil), send the whole body with 200
	bad := (*ByteRange)(nil)    // marker: ErrBadRange, 416
	cases := []struct {
		header string
		size   int64
		want   *ByteRange
	}{
		{"", size, whole},
		{"bytes=0-99", size, &ByteRange{0, 99}},
		{"bytes=100-", size, &ByteRange{100, 9999}},
		{"bytes=-100", size, &ByteRange{9900, 9999}},
		{"bytes=0-0", size, &ByteRange{0, 0}},
		{"bytes=9999-9999", size, &ByteRange{9999, 9999}},
		{"bytes=9990-20000", size, &ByteRange{9990, 9999}}, // end clamped to the last byte
		{"bytes=-20000", size, &ByteRange{0, 9999}},        // suffix longer than the body
		{"bytes= 0-99 ", size, &ByteRange{0, 99}},          // optional whitespace, as ServeContent
		{"bytes=0-1,5-6", size, whole},                     // multi-range ignored (§12 #10)
		{"bytes=10000-", size, bad},                        // start = size
		{"bytes=10000-10001", size, bad},
		{"bytes=-0", size, bad}, // RFC 9110: zero-length suffix is unsatisfiable
		{"bytes=5-2", size, bad},
		{"bytes=abc", size, bad},
		{"bytes=", size, bad},
		{"bytes=-", size, bad},
		{"bytes=1--2", size, bad},
		{"bytes=+1-2", size, bad}, // strconv would accept the sign; the RFC does not
		{"bytes=1-+2", size, bad},
		{"bytes=0x10-", size, bad},
		{"items=0-1", size, bad},
		{"bytes=99999999999999999999-", size, bad}, // overflows int64
		{"bytes=0-", 0, bad},                       // empty body: nothing is satisfiable
		{"bytes=-5", 0, bad},
		{"", 0, whole},
	}
	for _, c := range cases {
		got, err := ParseRange(c.header, c.size)
		switch c.want {
		case whole:
			if got != nil || err != nil {
				t.Errorf("%q: got %v, %v; want whole body", c.header, got, err)
			}
		case bad:
			if err != ErrBadRange {
				t.Errorf("%q: got %v, %v; want ErrBadRange", c.header, got, err)
			}
		default:
			if err != nil || got == nil || *got != *c.want {
				t.Errorf("%q: got %v, %v; want %v", c.header, got, err, *c.want)
			}
		}
	}
}

func TestByteRangeHeaders(t *testing.T) {
	b := ByteRange{100, 199}
	if b.Len() != 100 || b.ContentRange(10000) != "bytes 100-199/10000" {
		t.Fatalf("Len %d, ContentRange %q", b.Len(), b.ContentRange(10000))
	}
	if one := (ByteRange{0, 0}); one.Len() != 1 {
		t.Fatalf("bytes=0-0 has length %d, want 1", one.Len())
	}
}
