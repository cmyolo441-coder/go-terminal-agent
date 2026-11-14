package browsertools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func sp(s string) *string { return &s }

func sampleSnapshot() *Snapshot {
	return &Snapshot{
		URL: "https://shop.test/login", Title: "Log in", VW: 1280, VH: 720, SY: 0, SH: 2000,
		Nodes: []Node{
			{Ref: "e1", Role: "navigation", Name: "Main", Depth: 0, InView: true},
			{Ref: "e2", Role: "link", Name: "Home", Depth: 1, InView: true, Href: "/"},
			{Ref: "e3", Role: "heading", Name: "Log in", Level: 1, InView: true},
			{Ref: "e4", Role: "form", Depth: 0, InView: true},
			{Ref: "e5", Role: "textbox", Name: "Email", Depth: 1, InView: true, Required: true, Value: sp(""), Type: "email"},
			{Ref: "e6", Role: "textbox", Name: "Password", Depth: 1, InView: true, Value: sp("••••"), Type: "password"},
			{Ref: "e7", Role: "checkbox", Name: "Remember me", Depth: 1, InView: true, Checked: "false"},
			{Ref: "e8", Role: "combobox", Name: "Country", Depth: 1, InView: true, Value: sp("Germany"), Options: []string{"United States", "Germany"}, MoreOptions: 3},
			{Ref: "e9", Role: "button", Name: "Log in", Depth: 1, InView: true, Disabled: true},
			{Ref: "e10", Role: "button", Name: "Menu", Depth: 0, InView: true, Expanded: "false"},
			{Ref: "e11", Role: "link", Name: "Terms", Depth: 0, InView: false, Href: "https://other.test/terms"},
		},
		Total: 11,
	}
}

func TestNodeLine(t *testing.T) {
	s := sampleSnapshot()
	want := map[string]string{
		"e2":  `[e2] link "Home" -> /`,
		"e3":  `[e3] h1 "Log in"`,
		"e5":  `[e5] textbox "Email" (required) type=email value=""`,
		"e7":  `[e7] checkbox "Remember me" (unchecked)`,
		"e8":  `[e8] combobox "Country" value="Germany" options=[United States | Germany | …3 more]`,
		"e9":  `[e9] button "Log in" (disabled)`,
		"e10": `[e10] button "Menu" (collapsed)`,
		"e11": `[e11] link "Terms" (offscreen) -> https://other.test/terms`,
	}
	for _, n := range s.Nodes {
		if w, ok := want[n.Ref]; ok && n.Line() != w {
			t.Errorf("%s:\n got %s\nwant %s", n.Ref, n.Line(), w)
		}
	}
	n := Node{Ref: "e1", Role: "button", Name: `say "hi" <b>`, InView: true}
	if got := n.Line(); got != `[e1] button "say \"hi\" <b>"` {
		t.Errorf("quoting: %s", got)
	}
}

func TestRenderOutline(t *testing.T) {
	out := sampleSnapshot().Render(0)
	for _, want := range []string{
		"Page: Log in\nURL: https://shop.test/login\nViewport 1280x720, scrolled 0/1280px (0%)\n",
		"\n[e1] navigation \"Main\"\n  [e2] link \"Home\" -> /\n",
		"\n[e4] form\n  [e5] textbox",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}
	empty := (&Snapshot{URL: "about:blank", VW: 10, VH: 10}).Render(100)
	if !strings.Contains(empty, "(untitled)") || !strings.Contains(empty, "no visible interactive elements") {
		t.Errorf("empty render: %s", empty)
	}
}

func bigSnapshot(n int, inView func(i int) bool) *Snapshot {
	s := &Snapshot{URL: "https://big.test/", Title: "Big", VW: 800, VH: 600, SH: 90000}
	for i := 1; i <= n; i++ {
		s.Nodes = append(s.Nodes, Node{Ref: fmt.Sprintf("e%d", i), Role: "link", Name: fmt.Sprintf("Item number %d with a long name", i), InView: inView(i)})
	}
	s.Total = n
	return s
}

func TestRenderElidesOffscreenFirst(t *testing.T) {
	s := bigSnapshot(400, func(i int) bool { return i <= 20 })
	out := s.Render(4000)
	if utf8.RuneCountInString(out) > 4000 {
		t.Fatalf("render is %d chars, cap 4000", utf8.RuneCountInString(out))
	}
	if !strings.Contains(out, "[e20] link") {
		t.Fatal("in-view elements must survive")
	}
	if !strings.Contains(out, "… 380 offscreen elements (e21 … e400)") {
		t.Fatalf("offscreen run not summarized:\n%s", out)
	}
}

func TestRenderHardCap(t *testing.T) {
	s := bigSnapshot(2000, func(int) bool { return true })
	s.Truncated, s.Total = true, 5000
	for _, limit := range []int{600, 2000, 12000} {
		out := s.Render(limit)
		if n := utf8.RuneCountInString(out); n > limit {
			t.Fatalf("limit %d: got %d chars", limit, n)
		}
		if !strings.Contains(out, "more lines not shown") {
			t.Fatalf("limit %d: no truncation note", limit)
		}
		if !strings.Contains(out, "the page has 5000 elements; only the first 2000") {
			t.Fatalf("limit %d: no collection note", limit)
		}
		if !strings.HasPrefix(out, "Page: Big") {
			t.Fatal("header lost")
		}
	}
}

func TestDiff(t *testing.T) {
	old := sampleSnapshot()
	cur := sampleSnapshot()
	cur.Nodes[4].Value = sp("a@b.c") // e5 changed
	cur.Nodes = append(cur.Nodes[:8], cur.Nodes[9:]...)
	cur.Nodes = append(cur.Nodes, Node{Ref: "e12", Role: "dialog", Name: "Welcome", InView: true, Modal: true})
	cur.Nodes[10-1].InView = true // e11 scrolled into view: not a change
	cur.SY = 300
	d := Diff(old, cur, 3000)
	for _, want := range []string{
		"Scrolled to y=300",
		`~ [e5] textbox "Email" (required) type=email value="a@b.c"`,
		`+ [e12] dialog "Welcome" (modal)`,
		`- [e9] button "Log in" (disabled)`,
	} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "e11") {
		t.Errorf("visibility change reported as a change:\n%s", d)
	}
	if got := Diff(old, sampleSnapshot(), 3000); got != "No visible change to the page outline." {
		t.Errorf("no-change diff = %q", got)
	}
	nav := sampleSnapshot()
	nav.URL = "https://shop.test/account"
	if got := Diff(old, nav, 3000); !strings.HasPrefix(got, "Page is now:\nPage: Log in\nURL: https://shop.test/account") {
		t.Errorf("navigation diff = %q", got)
	}
	frag := sampleSnapshot()
	frag.URL += "#top"
	if got := Diff(old, frag, 3000); !strings.Contains(got, "URL: https://shop.test/login#top") || strings.Contains(got, "Page is now") {
		t.Errorf("fragment change = %q", got)
	}
	if Diff(old, nil, 100) != "" {
		t.Error("nil current should give no diff")
	}
}

func TestDiffCap(t *testing.T) {
	old := bigSnapshot(10, func(int) bool { return true })
	cur := bigSnapshot(500, func(int) bool { return true })
	d := Diff(old, cur, 1000)
	if utf8.RuneCountInString(d) > 1000 {
		t.Fatalf("diff is %d chars", utf8.RuneCountInString(d))
	}
	if !strings.Contains(d, "more changes; call browser_snapshot") {
		t.Fatalf("no cap note:\n%s", d)
	}
}

func TestSnapshotJSONDecoding(t *testing.T) {
	raw := `{"url":"u","title":"t","vw":1,"vh":2,"sx":0,"sy":5,"sw":1,"sh":9,"total":3,"truncated":true,"crossOriginFrames":1,
	"nodes":[{"ref":"e1","role":"checkbox","name":"A","depth":0,"box":[1,2,3,4],"inView":true,"checked":"mixed"},
	{"ref":"e2","role":"button","name":"B","depth":1,"box":[0,0,1,1],"inView":false,"pressed":true,"expanded":false},
	{"ref":"e3","role":"textbox","name":"C","depth":1,"box":[0,0,1,1],"inView":true,"value":"x","focused":true}]}`
	var s Snapshot
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if s.Nodes[0].Checked != "mixed" || s.Nodes[1].Pressed != "true" || s.Nodes[1].Expanded != "false" || *s.Nodes[2].Value != "x" {
		t.Fatalf("decoded %+v", s.Nodes)
	}
	out := s.Render(0)
	for _, want := range []string{"(mixed)", "(pressed, collapsed, offscreen)", "(focused) value=\"x\"", "1 cross-origin frame(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	var tri Tri
	if err := json.Unmarshal([]byte(`{}`), &tri); err == nil {
		t.Error("object should not decode as a tri-state")
	}
	b, _ := json.Marshal(struct{ A, B, C Tri }{"true", "", "mixed"})
	if string(b) != `{"A":true,"B":null,"C":"mixed"}` {
		t.Errorf("tri marshal = %s", b)
	}
}
