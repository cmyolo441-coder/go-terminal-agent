package browsertools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckURL(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<p>hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.html"), filepath.Join(root, "link.html")); err != nil {
		t.Fatal(err)
	}
	realRoot, _ := filepath.EvalSymlinks(root)
	fileIndex := "file://" + filepath.ToSlash(filepath.Join(realRoot, "index.html"))

	ok := map[string]string{
		"https://example.com/a?b=1":                   "https://example.com/a?b=1",
		"HTTP://Example.com":                          "http://Example.com",
		"example.com/path":                            "https://example.com/path",
		"localhost:3000/app":                          "http://localhost:3000/app",
		"127.0.0.1:8080":                              "http://127.0.0.1:8080",
		"http://localhost:5173/":                      "http://localhost:5173/",
		"about:blank":                                 "about:blank",
		"  https://x.org  ":                           "https://x.org",
		"index.html":                                  fileIndex,
		"./index.html":                                fileIndex,
		filepath.Join(root, "index.html"):             fileIndex,
		"file://" + filepath.Join(root, "index.html"): fileIndex,
	}
	for in, want := range ok {
		got, err := CheckURL(in, root)
		if err != nil {
			t.Errorf("CheckURL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("CheckURL(%q) = %q, want %q", in, got, want)
		}
	}
	bad := map[string]string{
		"":                                     "required",
		"javascript:alert(1)":                  "not allowed",
		"JavaScript:alert(1)":                  "not allowed",
		"data:text/html,<script>":              "data: scheme is not allowed",
		"chrome://settings":                    "chrome: scheme",
		"about:config":                         "about: scheme",
		"ftp://x.org":                          "ftp: scheme",
		"https://":                             "no host",
		"file:///etc/passwd":                   "outside the workspace",
		"/etc/passwd":                          "outside the workspace",
		"../x.html":                            "",
		"file://otherhost/x":                   "another host",
		"link.html":                            "outside the workspace",
		"http://a.org/\nx":                     "control characters",
		"not a url":                            "full http(s) URL",
		"~/x.html":                             "inside the workspace",
		"https://" + strings.Repeat("a", 9000): "longer than",
	}
	for in, want := range bad {
		_, err := CheckURL(in, root)
		if err == nil {
			t.Errorf("CheckURL(%q) should fail", in)
			continue
		}
		if want != "" && !strings.Contains(err.Error(), want) {
			t.Errorf("CheckURL(%q) error %q, want it to mention %q", in, err, want)
		}
	}
	if _, err := CheckURL("./index.html", ""); err == nil {
		t.Error("files must be refused without a workspace")
	}
}
