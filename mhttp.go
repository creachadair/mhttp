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

// Match is the parsed representation of an If-Match or If-None-Match header.
type Match struct {
	terms []term
}

type term struct {
	tag  string
	weak bool
}

// IsPresent reports whether a match header was present at the time of parsing.
func (m Match) IsPresent() bool { return m.terms != nil }

// IsGlob reports whether the match header value was a glob ("*").
func (m Match) IsGlob() bool { return m.terms != nil && len(m.terms) == 0 }

// Matches reports whether any of the tags in m match the specified etag using
// the "[strong]" comparison algorithm.
//
// If no match expression is present, the answer is always true.
// A glob match accepts any non-empty etag value.
// Otherwise, it reports whether etag is non-weak and exactly equal to
// one of the non-weak match tags, if any.
//
// The etag should have one of the following formats:
//
//	W/"value"
//	"value"
//	value
//
// The first is treated as a "weak" validator; the other two formats are
// treated as ordinary validators.
//
// [strong]: https://httpwg.org/specs/rfc9110.html#rfc.section.8.8.3.2
func (m Match) Matches(etag string) bool {
	if !m.IsPresent() {
		return true
	} else if m.IsGlob() {
		return etag != ""
	}
	clean, isWeak := trimTag(etag)
	if isWeak {
		return false
	}
	for _, term := range m.terms {
		if !term.weak && clean == term.tag {
			return true
		}
	}
	return false
}

// MatchesWeak reports whether any of the tags in m match the specified etag
// using the "[weak]" comparison algorithm.
//
// If no match expression is present, the answer is always true.
// A glob match accepts any non-empty etag value.
// Otherwise, it reports whether etag is exactly equal to one of the match tags
// disregarding whether etag or the match tags are weak.
//
// The etag should have one of the following formats:
//
//	W/"value"
//	"value"
//	value
//
// The first is treated as a "weak" validator; the other two formats are
// treated as ordinary validators.
//
// [weak]: https://httpwg.org/specs/rfc9110.html#rfc.section.8.8.3.2
func (m Match) MatchesWeak(etag string) bool {
	if !m.IsPresent() {
		return true
	} else if m.IsGlob() {
		return etag != ""
	}
	clean, _ := trimTag(etag)
	for _, term := range m.terms {
		if term.tag == clean {
			return true
		}
	}
	return false
}

func trimTag(s string) (_ string, isWeak bool) {
	tag, ok := strings.CutPrefix(strings.TrimSpace(s), "W/")
	return strings.TrimSuffix(strings.TrimPrefix(tag, `"`), `"`), ok
}

// ParseMatchHeader parses the contents of an HTTP If-Match or If-None-Match
// header and returns a [Match]. If the header is empty it returns a Match that
// matches all resources. Use [Match.IsPresent] to check for this case.
// Otherwise, see [Match.Matches] and [Match.MatchesWeak] for matching rules.
// An error is only reported if the header is present but invalid.
func ParseMatchHeader(s string) (Match, error) {
	clean := strings.TrimSpace(s)
	if clean == "" {
		return Match{}, nil // not present
	} else if clean == "*" {
		return Match{terms: []term{}}, nil // glob only
	}
	var terms []term
	for clean != "" {
		rest, isWeak := strings.CutPrefix(clean, "W/")
		q, rest, ok := cutQuoted(rest)
		if !ok {
			return Match{}, fmt.Errorf("invalid match term in %q", rest)
		}
		terms = append(terms, term{tag: q, weak: isWeak})
		clean, ok = strings.CutPrefix(strings.TrimSpace(rest), ",")
		if !ok && clean != "" {
			return Match{}, fmt.Errorf("extra text after term %q", clean)
		} else if ok && clean == "" {
			return Match{}, fmt.Errorf("missing term after %q", q)
		}
		clean = strings.TrimSpace(clean)
	}
	return Match{terms: terms}, nil
}

func cutQuoted(s string) (quoted, rest string, _ bool) {
	body, ok := strings.CutPrefix(s, `"`)
	if !ok {
		return "", s, false
	}
	for i, c := range body {
		if c == '"' {
			return body[:i], body[i+1:], true
		}
	}
	return "", s, false
}
