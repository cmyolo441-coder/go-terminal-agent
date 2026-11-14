package browsertools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Tri is an ARIA tri-state: "" (absent), "true", "false" or "mixed".
type Tri string

// UnmarshalJSON accepts true, false, "mixed" and null.
func (t *Tri) UnmarshalJSON(b []byte) error {
	switch s := string(bytes.TrimSpace(b)); s {
	case "true", "false":
		*t = Tri(s)
	case `"mixed"`:
		*t = "mixed"
	case "null", "":
		*t = ""
	default:
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return fmt.Errorf("tri-state: %s", s)
		}
		*t = Tri(str)
	}
	return nil
}

// MarshalJSON writes the tri-state back as true, false, "mixed" or null.
func (t Tri) MarshalJSON() ([]byte, error) {
	switch t {
	case "true", "false":
		return []byte(t), nil
	case "":
		return []byte("null"), nil
	}
	return json.Marshal(string(t))
}

// Node is one element of a snapshot.
type Node struct {
	Ref   string `json:"ref"`
	Role  string `json:"role"`
	Name  string `json:"name"`
	Depth int    `json:"depth"`
	// Box is x, y, width, height in top-viewport CSS pixels.
	Box    []float64 `json:"box"`
	InView bool      `json:"inView"`

	Value       *string  `json:"value,omitempty"`
	Checked     Tri      `json:"checked,omitempty"`
	Pressed     Tri      `json:"pressed,omitempty"`
	Expanded    Tri      `json:"expanded,omitempty"`
	Selected    Tri      `json:"selected,omitempty"`
	Disabled    bool     `json:"disabled,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Readonly    bool     `json:"readonly,omitempty"`
	Invalid     bool     `json:"invalid,omitempty"`
	Focused     bool     `json:"focused,omitempty"`
	Modal       bool     `json:"modal,omitempty"`
	Multiline   bool     `json:"multiline,omitempty"`
	Multiple    bool     `json:"multiple,omitempty"`
	ViaLabel    bool     `json:"viaLabel,omitempty"`
	CrossOrigin bool     `json:"crossOrigin,omitempty"`
	Level       int      `json:"level,omitempty"`
	Type        string   `json:"type,omitempty"`
	Href        string   `json:"href,omitempty"`
	Options     []string `json:"options,omitempty"`
	MoreOptions int      `json:"moreOptions,omitempty"`
}

// Snapshot is the page outline the snapshot script returns.
type Snapshot struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	// Viewport size and scroll position and extent of the main document.
	VW int `json:"vw"`
	VH int `json:"vh"`
	SX int `json:"sx"`
	SY int `json:"sy"`
	SW int `json:"sw"`
	SH int `json:"sh"`

	Nodes []Node `json:"nodes"`
	// Total counts every element that qualified, collected or not.
	Total             int  `json:"total"`
	Truncated         bool `json:"truncated"`
	CrossOriginFrames int  `json:"crossOriginFrames"`
	// Scope is the ref the snapshot was limited to, if any.
	Scope string `json:"-"`
}

// SnapshotOptions limit a snapshot.
type SnapshotOptions struct {
	// Scope limits the snapshot to the subtree of this ref.
	Scope string
	// ViewportOnly leaves out elements outside the viewport.
	ViewportOnly bool
	// MaxNodes caps collected elements; zero uses the session default.
	MaxNodes int
}

// takeSnapshot runs the snapshot script without logging or recording.
func (s *Session) takeSnapshot(ctx context.Context, d Driver, o SnapshotOptions) (*Snapshot, error) {
	max := o.MaxNodes
	if max <= 0 {
		max = s.opts.MaxSnapshotNodes
	}
	args := map[string]any{"max": max, "viewportOnly": o.ViewportOnly}
	if o.Scope != "" {
		args["scope"] = o.Scope
	}
	var snap Snapshot
	if err := callAgent(ctx, d, &snap, "snapshot", args); err != nil {
		return nil, err
	}
	snap.Scope = o.Scope
	return &snap, nil
}

// Snapshot captures the page outline and makes its refs current.
func (s *Session) Snapshot(ctx context.Context, o SnapshotOptions) (*Snapshot, error) {
	s.op.Lock()
	defer s.op.Unlock()
	d, err := s.Driver(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := s.takeSnapshot(ctx, d, o)
	if err != nil {
		s.record(Action{Kind: "snapshot", Target: o.Scope, Err: err.Error()})
		return nil, err
	}
	s.setSnapshot(snap)
	s.record(Action{Kind: "snapshot", Target: o.Scope, Detail: fmt.Sprintf("%d elements", len(snap.Nodes))})
	return snap, nil
}

// roleLabel renders the role, with a heading's level folded in ("h2").
func (n *Node) roleLabel() string {
	if n.Role == "heading" && n.Level >= 1 && n.Level <= 6 {
		return "h" + strconv.Itoa(n.Level)
	}
	return n.Role
}

// Line renders a node as one outline line without indentation, e.g.
//
//	[e12] textbox "Email" (required) value=""
func (n *Node) Line() string { return n.line(true) }

func (n *Node) line(withView bool) string {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(n.Ref)
	b.WriteString("] ")
	b.WriteString(n.roleLabel())
	if n.Name != "" {
		b.WriteString(" ")
		b.WriteString(quote(n.Name))
	}
	var flags []string
	switch n.Checked {
	case "true":
		flags = append(flags, "checked")
	case "false":
		flags = append(flags, "unchecked")
	case "mixed":
		flags = append(flags, "mixed")
	}
	switch n.Pressed {
	case "true":
		flags = append(flags, "pressed")
	case "mixed":
		flags = append(flags, "partly pressed")
	}
	switch n.Expanded {
	case "true":
		flags = append(flags, "expanded")
	case "false":
		flags = append(flags, "collapsed")
	}
	if n.Selected == "true" {
		flags = append(flags, "selected")
	}
	for _, f := range []struct {
		on   bool
		name string
	}{
		{n.Disabled, "disabled"}, {n.Required, "required"}, {n.Readonly, "readonly"},
		{n.Invalid, "invalid"}, {n.Focused, "focused"}, {n.Modal, "modal"},
		{n.Multiple, "multiple"}, {n.CrossOrigin, "cross-origin"},
	} {
		if f.on {
			flags = append(flags, f.name)
		}
	}
	if withView && !n.InView && !isContainer(n.Role) {
		flags = append(flags, "offscreen")
	}
	if len(flags) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(flags, ", "))
		b.WriteString(")")
	}
	if n.Type != "" {
		b.WriteString(" type=")
		b.WriteString(n.Type)
	}
	if n.Value != nil {
		b.WriteString(" value=")
		b.WriteString(quote(*n.Value))
	}
	if len(n.Options) > 0 {
		b.WriteString(" options=[")
		b.WriteString(strings.Join(n.Options, " | "))
		if n.MoreOptions > 0 {
			fmt.Fprintf(&b, " | …%d more", n.MoreOptions)
		}
		b.WriteString("]")
	}
	if n.Href != "" && n.Href != n.Name {
		b.WriteString(" -> ")
		b.WriteString(n.Href)
	}
	return b.String()
}

var containerRoles = map[string]bool{
	"banner": true, "navigation": true, "main": true, "complementary": true, "contentinfo": true,
	"region": true, "form": true, "search": true, "dialog": true, "alertdialog": true, "group": true,
	"menu": true, "menubar": true, "listbox": true, "tablist": true, "tree": true, "grid": true,
	"radiogroup": true, "toolbar": true, "iframe": true,
}

func isContainer(role string) bool { return containerRoles[role] }

func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// header renders the page header lines.
func (s *Snapshot) header() string {
	var b strings.Builder
	title := s.Title
	if title == "" {
		title = "(untitled)"
	}
	fmt.Fprintf(&b, "Page: %s\nURL: %s\n", title, s.URL)
	scroll := ""
	if s.SH > s.VH && s.VH > 0 {
		pct := 100
		if max := s.SH - s.VH; max > 0 {
			pct = s.SY * 100 / max
		}
		scroll = fmt.Sprintf(", scrolled %d/%dpx (%d%%)", s.SY, s.SH-s.VH, min(pct, 100))
	}
	fmt.Fprintf(&b, "Viewport %dx%d%s\n", s.VW, s.VH, scroll)
	if s.Scope != "" {
		fmt.Fprintf(&b, "Scope: %s\n", s.Scope)
	}
	return b.String()
}

// Render writes the snapshot as an indented outline of at most maxChars
// characters (zero means no cap). When it does not fit, runs of offscreen
// elements are summarized first, then the tail is cut.
func (s *Snapshot) Render(maxChars int) string {
	head := s.header()
	if len(s.Nodes) == 0 {
		return head + "\n(no visible interactive elements)\n"
	}
	full := renderNodes(s.Nodes, false)
	out := head + "\n" + full + s.footer(0)
	if maxChars <= 0 || utf8.RuneCountInString(out) <= maxChars {
		return out
	}
	compact := renderNodes(s.Nodes, true)
	out = head + "\n" + compact + s.footer(0)
	if utf8.RuneCountInString(out) <= maxChars {
		return out
	}
	// Hard cut on line boundaries, keeping room for the footer.
	lines := strings.SplitAfter(compact, "\n")
	budget := maxChars - utf8.RuneCountInString(head) - 1 - 220
	var b strings.Builder
	used, kept := 0, 0
	for _, l := range lines {
		n := utf8.RuneCountInString(l)
		if used+n > budget {
			break
		}
		b.WriteString(l)
		used += n
		kept++
	}
	omitted := 0
	for _, l := range lines[kept:] {
		if strings.TrimSpace(l) != "" {
			omitted++
		}
	}
	return head + "\n" + b.String() + s.footer(omitted)
}

func (s *Snapshot) footer(cutLines int) string {
	var notes []string
	if cutLines > 0 {
		notes = append(notes, fmt.Sprintf("… %d more lines not shown (output capped). Use browser_snapshot with ref=<container ref> or viewport_only=true to focus.", cutLines))
	}
	if s.Truncated && s.Total > len(s.Nodes) {
		notes = append(notes, fmt.Sprintf("… the page has %d elements; only the first %d were collected.", s.Total, len(s.Nodes)))
	}
	if s.CrossOriginFrames > 0 {
		notes = append(notes, fmt.Sprintf("Note: %d cross-origin frame(s) cannot be inspected.", s.CrossOriginFrames))
	}
	if len(notes) == 0 {
		return ""
	}
	return strings.Join(notes, "\n") + "\n"
}

// renderNodes renders the outline; with elide set, runs of offscreen leaf
// elements collapse into one summary line.
func renderNodes(nodes []Node, elide bool) string {
	var b strings.Builder
	for i := 0; i < len(nodes); i++ {
		n := &nodes[i]
		if elide && !n.InView && !isContainer(n.Role) {
			j := i
			for j < len(nodes) && !nodes[j].InView && !isContainer(nodes[j].Role) && nodes[j].Depth == n.Depth {
				j++
			}
			if j-i >= 2 {
				writeIndent(&b, n.Depth)
				fmt.Fprintf(&b, "… %d offscreen elements (%s … %s)\n", j-i, n.Ref, nodes[j-1].Ref)
				i = j - 1
				continue
			}
		}
		writeIndent(&b, n.Depth)
		b.WriteString(n.line(true))
		b.WriteString("\n")
	}
	return b.String()
}

func writeIndent(b *strings.Builder, depth int) {
	for range min(depth, 12) {
		b.WriteString("  ")
	}
}

// Diff summarizes how the page changed from old to cur in at most maxChars
// characters. A different URL renders cur in full; otherwise the
// summary lists added (+), changed (~) and removed (-) elements. Scrolling
// alone is not a change of any element.
func Diff(old, cur *Snapshot, maxChars int) string {
	if cur == nil {
		return ""
	}
	if old == nil || stripFragment(old.URL) != stripFragment(cur.URL) {
		return "Page is now:\n" + cur.Render(maxChars)
	}
	var lines []string
	if old.Title != cur.Title {
		lines = append(lines, "Title: "+quote(cur.Title))
	}
	if old.URL != cur.URL {
		lines = append(lines, "URL: "+cur.URL)
	}
	if old.SY != cur.SY || old.SX != cur.SX {
		lines = append(lines, fmt.Sprintf("Scrolled to y=%d (of %d)", cur.SY, max(cur.SH-cur.VH, 0)))
	}
	prev := make(map[string]string, len(old.Nodes))
	for i := range old.Nodes {
		prev[old.Nodes[i].Ref] = old.Nodes[i].line(false)
	}
	seen := make(map[string]bool, len(cur.Nodes))
	var changes []string
	for i := range cur.Nodes {
		n := &cur.Nodes[i]
		seen[n.Ref] = true
		l := n.line(false)
		p, ok := prev[n.Ref]
		switch {
		case !ok:
			changes = append(changes, "+ "+n.line(true))
		case p != l:
			changes = append(changes, "~ "+n.line(true))
		}
	}
	for i := range old.Nodes {
		if !seen[old.Nodes[i].Ref] {
			changes = append(changes, "- "+old.Nodes[i].line(false))
		}
	}
	if len(changes) == 0 && len(lines) == 0 {
		return "No visible change to the page outline."
	}
	var b strings.Builder
	b.WriteString("Page changes:\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	for i, c := range changes {
		if maxChars > 0 && utf8.RuneCountInString(b.String())+utf8.RuneCountInString(c) > maxChars-80 {
			fmt.Fprintf(&b, "… %d more changes; call browser_snapshot for the full page.\n", len(changes)-i)
			break
		}
		b.WriteString(c)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func stripFragment(u string) string {
	if i := strings.IndexByte(u, '#'); i >= 0 {
		return u[:i]
	}
	return u
}
