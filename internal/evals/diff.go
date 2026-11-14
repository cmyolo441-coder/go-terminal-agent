package evals

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DiffKind classifies one structural difference.
type DiffKind string

const (
	DiffAdded   DiffKind = "added"   // present in the actual request only
	DiffRemoved DiffKind = "removed" // present in the recorded request only
	DiffChanged DiffKind = "changed" // present in both with different values
)

// Difference is one structural difference between a recorded request and
// the request actually made, addressed by a dotted path.
type Difference struct {
	Path     string   `json:"path"`
	Kind     DiffKind `json:"kind"`
	Recorded string   `json:"recorded,omitempty"`
	Actual   string   `json:"actual,omitempty"`
}

func (d Difference) String() string {
	switch d.Kind {
	case DiffAdded:
		return fmt.Sprintf("+ %s: %s", d.Path, d.Actual)
	case DiffRemoved:
		return fmt.Sprintf("- %s: %s", d.Path, d.Recorded)
	default:
		return fmt.Sprintf("~ %s: %s -> %s", d.Path, d.Recorded, d.Actual)
	}
}

// maxDiffs bounds the work and output of a single structural diff.
const maxDiffs = 200

// structuralDiff compares two canonical trees. Arrays are compared by
// index; strings that differ are summarised around the first differing
// rune so long prompts produce readable output.
func structuralDiff(recorded, actual any) []Difference {
	var out []Difference
	diffInto(&out, "", recorded, actual)
	return out
}

func joinPath(base, seg string) string {
	if base == "" {
		return seg
	}
	return base + "." + seg
}

func diffInto(out *[]Difference, path string, a, b any) {
	if len(*out) >= maxDiffs {
		return
	}
	root := path
	if root == "" {
		root = "$"
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			*out = append(*out, Difference{Path: root, Kind: DiffChanged, Recorded: summarize(a), Actual: summarize(b)})
			return
		}
		keys := make([]string, 0, len(av)+len(bv))
		for k := range av {
			keys = append(keys, k)
		}
		for k := range bv {
			if _, dup := av[k]; !dup {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			ae, aok := av[k]
			be, bok := bv[k]
			p := joinPath(path, k)
			switch {
			case aok && !bok:
				*out = append(*out, Difference{Path: p, Kind: DiffRemoved, Recorded: summarize(ae)})
			case !aok && bok:
				*out = append(*out, Difference{Path: p, Kind: DiffAdded, Actual: summarize(be)})
			default:
				diffInto(out, p, ae, be)
			}
			if len(*out) >= maxDiffs {
				return
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			*out = append(*out, Difference{Path: root, Kind: DiffChanged, Recorded: summarize(a), Actual: summarize(b)})
			return
		}
		n := max(len(av), len(bv))
		for i := 0; i < n; i++ {
			p := joinPath(path, strconv.Itoa(i))
			switch {
			case i >= len(bv):
				*out = append(*out, Difference{Path: p, Kind: DiffRemoved, Recorded: summarize(av[i])})
			case i >= len(av):
				*out = append(*out, Difference{Path: p, Kind: DiffAdded, Actual: summarize(bv[i])})
			default:
				diffInto(out, p, av[i], bv[i])
			}
			if len(*out) >= maxDiffs {
				return
			}
		}
	case string:
		bs, ok := b.(string)
		if !ok {
			*out = append(*out, Difference{Path: root, Kind: DiffChanged, Recorded: summarize(a), Actual: summarize(b)})
			return
		}
		if av != bs {
			r, x := stringDelta(av, bs)
			*out = append(*out, Difference{Path: root, Kind: DiffChanged, Recorded: r, Actual: x})
		}
	default:
		if !leafEqual(a, b) {
			*out = append(*out, Difference{Path: root, Kind: DiffChanged, Recorded: summarize(a), Actual: summarize(b)})
		}
	}
}

func leafEqual(a, b any) bool {
	switch av := a.(type) {
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && normalizeNumber(av) == normalizeNumber(bv)
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	default:
		ar, _ := canonicalJSON(a)
		br, _ := canonicalJSON(b)
		return string(ar) == string(br)
	}
}

const (
	summaryLimit = 120
	deltaContext = 40
)

// summarize renders a value compactly for a diff line.
func summarize(v any) string {
	raw, err := canonicalJSON(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return truncateRunes(string(raw), summaryLimit)
}

// stringDelta returns quoted excerpts of a and b around their first
// differing rune, with ellipses marking elided text.
func stringDelta(a, b string) (string, string) {
	if utf8.RuneCountInString(a) <= summaryLimit && utf8.RuneCountInString(b) <= summaryLimit {
		return strconv.Quote(a), strconv.Quote(b)
	}
	ar, br := []rune(a), []rune(b)
	i := 0
	for i < len(ar) && i < len(br) && ar[i] == br[i] {
		i++
	}
	start := max(0, i-deltaContext)
	excerpt := func(r []rune) string {
		end := min(len(r), i+deltaContext)
		s := strconv.Quote(string(r[start:end]))
		if start > 0 {
			s = "…" + s
		}
		if end < len(r) {
			s += "…"
		}
		return fmt.Sprintf("%s (len %d, differs at %d)", s, len(r), i)
	}
	return excerpt(ar), excerpt(br)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// formatDiffs renders at most limit differences, one per line.
func formatDiffs(diffs []Difference, limit int) string {
	var b strings.Builder
	for i, d := range diffs {
		if i == limit {
			fmt.Fprintf(&b, "  … %d more difference(s)\n", len(diffs)-limit)
			break
		}
		b.WriteString("  ")
		b.WriteString(d.String())
		b.WriteByte('\n')
	}
	return b.String()
}
