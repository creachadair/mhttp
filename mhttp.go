// Package mhttp exports utilities for implementing HTTP clients and services.
package mhttp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Range represents a single byte range of an underlying resource.
// The positions are zero-indexed with Start inclusive and End exclusive.
type Range struct {
	Start, End int64
}

// Span reports the number of bytes spanned by r.
func (r Range) Span() int64 { return r.End - r.Start }

// ContentRange returns the contents of a content-range header for r given the
// specified total resource size.
func (r Range) ContentRange(size int) string {
	return fmt.Sprintf("bytes %d-%d/%d", r.Start, r.End-1, size)
}

// ParseRangeHeader parses the contents of an HTTP [Range] header for a
// resource of the specified size in bytes. On success, the resulting ranges
// are adjusted to absolute offsets within the resource.
//
// If s == "", it returns empty without error, indicating the entire resource
// is requested in a single range.
//
// [Range]: https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Range
func ParseRangeHeader(size int64, s string) ([]Range, error) {
	if s == "" {
		return nil, nil // no ranges are requested
	}

	// Grammar: bytes=lo-hi bytes=lo- bytes=-hi bytes=lo1-hi1,lo2-hi2,...
	kind, rest, ok := strings.Cut(s, "=")
	if !ok {
		return nil, errors.New("invalid range syntax")
	} else if kind != "bytes" {
		return nil, fmt.Errorf("invalid range type %q", kind)
	}

	var out []Range
	for rs := range strings.SplitSeq(rest, ",") {
		lo, hi, ok := strings.Cut(strings.TrimSpace(rs), "-")
		if !ok || lo == "" && hi == "" {
			return nil, fmt.Errorf("invalid range format %q", rs)
		}

		vlo, err := strconv.ParseInt(lo, 10, 64)
		if err != nil && lo != "" || vlo < 0 {
			return nil, fmt.Errorf("invalid range start %q: %w", lo, err)
		}
		vhi, err := strconv.ParseInt(hi, 10, 64)
		if err != nil && hi != "" || vhi < 0 {
			return nil, fmt.Errorf("invalid range end %q: %w", hi, err)
		}
		// Reaching here, vlo and vhi are valid range endpoints if present, but
		// may not be correctly bounded for size.

		switch {
		case lo == "": // -hi → (size-hi)..size
			if vhi > size {
				return nil, fmt.Errorf("span %d exceeds size %d", vhi, size)
			}
			out = append(out, Range{Start: size - vhi, End: size})
		case hi == "": // lo- → lo..size
			out = append(out, Range{Start: vlo, End: size})
		default:
			out = append(out, Range{Start: vlo, End: min(vhi+1, size)})
			// +1 to make the range exclusive; min to cap at the actual size
		}
		if st := out[len(out)-1].Start; st > size {
			return nil, fmt.Errorf("range %d: start %d > size %d", len(out), st, size)
		}
	}
	return out, nil
}
