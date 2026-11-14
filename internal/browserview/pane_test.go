package browserview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type fakeSource struct {
	ch  chan Frame
	err error
}

func newFakeSource() *fakeSource { return &fakeSource{ch: make(chan Frame, 64)} }

func (s *fakeSource) Subscribe(ctx context.Context) (<-chan Frame, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.ch, nil
}

func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("nil cmd")
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case m := <-ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("command did not complete")
	}
	return nil
}

func testFrame(t testing.TB, seq uint64, c color.RGBA) Frame {
	img := gradient(160, 100)
	for x := 0; x < 20; x++ {
		img.SetRGBA(x, 0, c)
	}
	return Frame{Data: encodePNG(t, img), Format: "png", Width: 160, Height: 100, Seq: seq, Timestamp: time.Now()}
}

func newTestPane(src Source, proto Protocol) *Pane {
	return New(src, PaneOptions{
		Title:        "browser",
		Capabilities: &Capabilities{Protocol: proto, TrueColor: true, Cell: CellSize{W: 8, H: 16}},
		MaxFPS:       240,
	})
}

func TestPaneHalfBlockView(t *testing.T) {
	src := newFakeSource()
	p := newTestPane(src, ProtocolHalfBlock)
	defer p.Close()
	p.SetSize(64, 16)
	p.SetOverlay(Overlay{URL: "https://example.com/path", Label: "click #submit", Status: "navigating"})
	p.Focus()

	// Before any frame the pane shows a placeholder.
	v := p.View()
	if !strings.Contains(v, "waiting for first frame") {
		t.Fatalf("placeholder missing:\n%s", v)
	}

	cmd := p.Init()
	src.ch <- testFrame(t, 1, red)
	msg := runCmd(t, cmd)
	pm, ok := msg.(PaneMsg)
	if !ok || !pm.For(p) {
		t.Fatalf("unexpected msg %T", msg)
	}
	p, next := p.Update(msg)
	if next == nil {
		t.Fatal("Update must keep listening")
	}
	v = p.View()
	lines := strings.Split(v, "\n")
	if len(lines) != 16 {
		t.Fatalf("view has %d lines, want 16", len(lines))
	}
	for i, l := range lines {
		if w := StringWidth(l); w != 64 {
			t.Fatalf("line %d width %d: %q", i, w, l)
		}
	}
	for _, want := range []string{"browser", "https://example.com/path", "blocks", "click #submit", "navigating", "▀", "╭", "╯"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q", want)
		}
	}
	if st := p.Stats(); st.Seq != 1 || st.Protocol != ProtocolHalfBlock {
		t.Fatalf("stats = %+v", st)
	}
	if p.Graphics(true) != "" {
		t.Fatal("half-block pane has no out-of-band graphics")
	}

	// Messages for other panes are ignored.
	other := newTestPane(src, ProtocolHalfBlock)
	defer other.Close()
	if _, cmd := other.Update(msg); cmd != nil {
		t.Fatal("foreign PaneMsg must be ignored")
	}
}

func TestPaneSlowConsumerSeesNewestFrame(t *testing.T) {
	src := newFakeSource()
	p := newTestPane(src, ProtocolHalfBlock)
	defer p.Close()
	p.SetSize(40, 12)
	cmd := p.Init()
	const n = 40
	for i := uint64(1); i <= n; i++ {
		src.ch <- testFrame(t, i, color.RGBA{uint8(i * 5), 0, 0, 255})
	}
	// Do not consume events until the worker has caught up.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ev, ok := p.events.Peek(); ok && ev.r != nil && ev.r.Seq == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never rendered the newest frame")
		}
		time.Sleep(5 * time.Millisecond)
	}
	msg := runCmd(t, cmd)
	p, _ = p.Update(msg)
	if p.Stats().Seq != n {
		t.Fatalf("first delivered frame = %d, want newest %d", p.Stats().Seq, n)
	}
	if p.events.Dropped() == 0 {
		t.Fatal("expected intermediate renders to be coalesced")
	}
}

func TestPaneKittyGraphics(t *testing.T) {
	src := newFakeSource()
	p := newTestPane(src, ProtocolKitty)
	p.SetSize(42, 14)
	p.SetOrigin(3, 50)
	cmd := p.Init()
	src.ch <- testFrame(t, 1, red)
	p, cmd = p.Update(runCmd(t, cmd))

	v := p.View()
	if strings.Contains(v, "▀") || strings.Contains(v, "\x1b_G") {
		t.Fatal("image protocols must not put pixels in View")
	}
	l := p.cur.Layout
	g := p.Graphics(false)
	wantPos := fmt.Sprintf("\x1b7\x1b[%d;%dH\x1b_Ga=T,", 3+BodyRowOffset+l.OffsetRow+1, 50+BodyColOffset+l.OffsetCol+1)
	if !strings.HasPrefix(g, wantPos) || !strings.HasSuffix(g, "\x1b8") {
		t.Fatalf("graphics = %q..., want prefix %q", g[:min(len(g), 40)], wantPos)
	}
	if p.Graphics(false) != "" {
		t.Fatal("graphics must be emitted once per frame")
	}
	if p.Graphics(true) == "" {
		t.Fatal("force must re-emit")
	}
	firstID := p.cur.KittyID

	src.ch <- testFrame(t, 2, blue)
	p, _ = p.Update(runCmd(t, cmd))
	g = p.Graphics(false)
	del := string(KittyDeleteSeq(firstID, false))
	if !strings.Contains(g, del) || strings.Index(g, del) < strings.Index(g, "\x1b_Ga=T") {
		t.Fatal("previous image must be deleted after the new one is placed")
	}

	// WrapOutput appends pending graphics after a flush.
	var buf bytes.Buffer
	w := p.WrapOutput(&buf)
	if _, err := w.Write([]byte("text")); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "text" {
		t.Fatalf("nothing pending, got %q", buf.String())
	}
	buf.Reset()
	_, _ = w.Write([]byte("\x1b[2J"))
	if !strings.Contains(buf.String(), "\x1b_Ga=T") {
		t.Fatal("screen clear must trigger a redraw")
	}
	cleanup := p.Close()
	if !strings.Contains(cleanup, "a=d,d=I") {
		t.Fatalf("Close cleanup = %q", cleanup)
	}
	if p.Close() != "" {
		t.Fatal("Close must be idempotent")
	}
}

func TestPaneSixelRedrawsAfterTextFlush(t *testing.T) {
	src := newFakeSource()
	p := newTestPane(src, ProtocolSixel)
	defer p.Close()
	p.SetSize(30, 10)
	cmd := p.Init()
	src.ch <- testFrame(t, 1, red)
	p, _ = p.Update(runCmd(t, cmd))
	var buf bytes.Buffer
	w := p.WrapOutput(&buf)
	_, _ = w.Write([]byte("\x1b[?25l"))
	if !strings.Contains(buf.String(), "\x1bP0;1;0q") {
		t.Fatal("pending sixel must be written on the next flush")
	}
	buf.Reset()
	_, _ = w.Write([]byte("\x1b[?25l"))
	if strings.Contains(buf.String(), "\x1bP") {
		t.Fatal("pure control flush must not redraw")
	}
	buf.Reset()
	_, _ = w.Write([]byte("\x1b[5;1Hsome text"))
	if !strings.Contains(buf.String(), "\x1bP0;1;0q") {
		t.Fatal("text repaint must redraw sixel")
	}
	// Hiding blanks the area.
	p.SetVisible(false)
	buf.Reset()
	_, _ = w.Write([]byte("x"))
	if !strings.Contains(buf.String(), "X") || strings.Contains(buf.String(), "\x1bP0;1;0q") {
		t.Fatalf("hide should erase, got %q", buf.String())
	}
}

func TestPaneOverlayAndResizeRerender(t *testing.T) {
	src := newFakeSource()
	p := newTestPane(src, ProtocolHalfBlock)
	defer p.Close()
	p.SetSize(40, 12)
	cmd := p.Init()
	src.ch <- testFrame(t, 1, red)
	p, cmd = p.Update(runCmd(t, cmd))
	before := p.cur.Lines

	p.SetOverlay(Overlay{Cursor: &image.Point{80, 50}})
	// The cursor animates from its first position, so wait for a frame
	// that differs.
	p, cmd = p.Update(runCmd(t, cmd))
	if strings.Join(p.cur.Lines, "") == strings.Join(before, "") {
		t.Fatal("cursor overlay did not change the picture")
	}
	p.SetSize(60, 20)
	for i := 0; i < 20 && p.cur.Layout.Cols <= 38; i++ {
		p, cmd = p.Update(runCmd(t, cmd))
	}
	if p.cur.Layout.Cols <= 38 {
		t.Fatalf("resize not applied: %+v", p.cur.Layout)
	}
}

func TestPaneSubscribeError(t *testing.T) {
	src := &fakeSource{err: errors.New("cdp gone")}
	p := newTestPane(src, ProtocolHalfBlock)
	defer p.Close()
	p.SetSize(40, 10)
	p, _ = p.Update(runCmd(t, p.Init()))
	v := p.View()
	if !strings.Contains(v, "cdp gone") {
		t.Fatalf("error not shown:\n%s", v)
	}
}

func TestPaneSourceEnded(t *testing.T) {
	src := newFakeSource()
	p := newTestPane(src, ProtocolHalfBlock)
	defer p.Close()
	p.SetSize(40, 10)
	cmd := p.Init()
	close(src.ch)
	p, _ = p.Update(runCmd(t, cmd))
	if !strings.Contains(p.View(), "stream ended") {
		t.Fatalf("ended state missing:\n%s", p.View())
	}
}

func TestPaneTinySizeAndNilSource(t *testing.T) {
	p := New(nil, PaneOptions{Capabilities: &Capabilities{Protocol: ProtocolHalfBlock}})
	if p.Init() != nil {
		t.Fatal("nil source must not start")
	}
	p.SetSize(3, 3)
	if p.View() != "" {
		t.Fatal("too small to render")
	}
	p.SetSize(30, 8)
	if !strings.Contains(p.View(), "no browser attached") {
		t.Fatal("nil source placeholder")
	}
}
