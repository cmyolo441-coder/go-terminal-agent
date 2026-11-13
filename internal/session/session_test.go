package session

import (
	"testing"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

func TestSaveLoadList(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	a := New("/w/a", "m")
	a.Messages = []llm.Message{llm.UserText("fix the flaky test in   parser")}
	if err := st.Save(a); err != nil {
		t.Fatal(err)
	}
	b := New("/w/b", "m")
	b.ID = "zz-other"
	b.Messages = []llm.Message{llm.UserText("hello")}
	if err := st.Save(b); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(a.ID[:10])
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "fix the flaky test in parser" || len(got.Messages) != 1 {
		t.Fatalf("unexpected session: %+v", got)
	}
	list, _ := st.List("/w/a")
	if len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("list filter failed: %+v", list)
	}
	latest, err := st.Latest("/w/b")
	if err != nil || latest.ID != "zz-other" {
		t.Fatalf("latest: %v %v", latest, err)
	}
	if _, err := st.Load("nope"); err == nil {
		t.Fatal("expected not found")
	}
}

func TestEmptySessionNotSaved(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	if err := st.Save(New("/w", "m")); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.List(""); len(list) != 0 {
		t.Fatal("empty session should not be written")
	}
}
