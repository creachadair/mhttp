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

// Size reports the number of bytes spanned by r.
func (r Range) Size() int64 { return r.End - r.Start }

// String returns the representation of r as it appears in a Range header.
func (r Range) String() string { return fmt.Sprintf("%d-%d", r.Start, r.End-1) }

// ContentRange returns the contents of a content-range header for r given the
// specified total resource size.
func (r Range) ContentRange(totalSize int64) string {
	return fmt.Sprintf("bytes %d-%d/%d", r.Start, r.End-1, totalSize)
}

// ParseRangeHeader parses the contents of an HTTP [Range] header for a
// resource of the specified total size in bytes. On success, the resulting
// ranges are adjusted to absolute offsets within the resource.
// Ranges that start within the total size are clipped to fit, even if their
// specified endpoint is greater.
//
// If s == "", it returns empty without error, indicating the entire resource
// is requested in a single range.
//
// [Range]: https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Range
func ParseRangeHeader(totalSize int64, s string) ([]Range, error) {
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
		// may not be correctly bounded for totalSize.

		switch {
		case lo == "": // -hi → (size-hi)..size
			if vhi > totalSize {
				return nil, fmt.Errorf("span %d exceeds size %d", vhi, totalSize)
			}
			out = append(out, Range{Start: totalSize - vhi, End: totalSize})
		case hi == "": // lo- → lo..size
			out = append(out, Range{Start: vlo, End: totalSize})
		default:
			out = append(out, Range{Start: vlo, End: min(vhi+1, totalSize)})
			// +1 to make the range exclusive; min to cap at the actual size
		}
		if st := out[len(out)-1].Start; st > totalSize {
			return nil, fmt.Errorf("range %d: start %d > size %d", len(out), st, totalSize)
		}
	}
	return out, nil
}
