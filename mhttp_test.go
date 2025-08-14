package mhttp_test

import (
	"strings"
	"testing"

	"github.com/creachadair/mhttp"
	"github.com/google/go-cmp/cmp"
)

func TestParseRangeHeader(t *testing.T) {
	tests := []struct {
		name, input string
		size        int64
		want        []mhttp.Range
	}{
		{"Empty0", "", 0, nil},
		{"Empty100", "", 100, nil},
		{"Single", "bytes=0-15", 100, tr(0, 16)},
		{"StartOnly", "bytes=25-", 100, tr(25, 100)},
		{"SuffixLength", "bytes=-25", 100, tr(75, 100)},
		{"Multiple", "bytes=0-9,20-24,-30", 100, tr(0, 10, 20, 25, 70, 100)},
		{"PinEnd", "bytes=30-90", 50, tr(30, 50)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := mhttp.ParseRangeHeader(tc.size, tc.input)
			if err != nil {
				t.Errorf("ParseRangeHeader(%d, %q): unexpected error: %v", tc.size, tc.input, err)
			}
			if diff := cmp.Diff(rs, tc.want); diff != "" {
				t.Errorf("Ranges (-got, +want):\n%s", diff)
			}
		})
	}

	t.Run("Fail", func(t *testing.T) {
		tests := []struct {
			name, input string
			size        int64
			want        string
		}{
			{"Syntax", "bogus", 100, "invalid range syntax"},
			{"Type", "words=5-10", 99, `invalid range type "words"`},
			{"Format1", "bytes=", 99, "invalid range format"},
			{"Format2", "bytes=-", 99, "invalid range format"},
			{"BadStart", "bytes=bad-10", 99, "invalid range start"},
			{"BadEnd", "bytes=10-bad", 99, "invalid range end"},
			{"NegativeEnd", "bytes=10--93", 99, "invalid range end"},
			{"SuffixTooLong", "bytes=-100", 50, "span 100 exceeds size 50"},
			{"StartTooBig", "bytes=50-70", 25, "start 50 > size 25"},
			{"MultiFormat1", "bytes=1-5,", 25, "invalid range format"},
			{"MultiFormat2", "bytes=1-5,-", 25, "invalid range format"},
			{"MultiTooBig", "bytes=0-10,50-70", 25, "range 2: start 50 > size 25"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				rs, err := mhttp.ParseRangeHeader(tc.size, tc.input)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("ParseRangeHeader(%d, %q): got (%+v, %v), want error %q", tc.size, tc.input, rs, err, tc.want)
				}
			})
		}
	})
}

func tr(vs ...int) []mhttp.Range {
	var out []mhttp.Range
	for i := 0; i+1 < len(vs); i += 2 {
		out = append(out, mhttp.Range{Start: int64(vs[i]), End: int64(vs[i+1])})
	}
	return out
}

func TestMatch(t *testing.T) {
	tests := []struct {
		header       string
		etag         string
		strong, weak bool
	}{
		{"", "", true, true},
		{"", "apple", true, true},
		{"*", "", false, false},
		{"*", "pear", true, true},

		// Without quoting (non-standard, but tolerated).
		{"plum, cherry", "plum", true, true},
		{"plum, cherry", "quince", false, false},
		{"plum, cherry", "", false, false},

		// With quoting.
		{`"apple", "pear"`, "apple", true, true},
		{`"apple", "pear"`, `"pear"`, true, true},
		{`"apple", "pear"`, "plum", false, false},
		{`"apple", "pear"`, `"plum"`, false, false},
		{`"apple", pear`, "apple", true, true},
		{`"apple", pear`, "pear", true, true},

		// With "weak" prefixes.
		{`W/"apple"`, `"apple"`, false, true},
		{`"apple"`, `W/"apple"`, false, true},
		{`W/"pear"`, `W/"pear"`, false, true},
		{`W/"pear"`, `W/"plum"`, false, false},
		{`"apple", W/"pear", "plum"`, `"pear"`, false, true},
	}
	for _, tc := range tests {
		m := mhttp.ParseMatchHeader(tc.header)
		if got := m.Matches(tc.etag); got != tc.strong {
			t.Errorf("Strong %#q match %#q: got %v, want %v", tc.header, tc.etag, got, tc.strong)
		}
		if got := m.MatchesWeak(tc.etag); got != tc.weak {
			t.Errorf("Weak %#q match %#q: got %v, want %v", tc.header, tc.etag, got, tc.weak)
		}
	}
}
