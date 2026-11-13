package webtools

import (
	"net/url"
	"strings"
	"testing"
)

func TestHTMLToMarkdown(t *testing.T) {
	src := `<!doctype html><html><head><title>  Go   Docs </title>
<style>body{color:red}</style><script>alert("x")</script><base href="https://go.dev/doc/"></head>
<body>
<nav><a href="/">Home</a> | <a href="/blog">Blog</a></nav>
<div role="banner">Site banner</div>
<main>
<h1>Getting <em>started</em></h1>
<p>Install   Go from <a href="install">the download page</a>, then run <code>go version</code>.<br>Next line.</p>
<p>Use <strong>modules</strong> and <b> bold </b>text, <del>not GOPATH</del>.</p>
<h2>Steps</h2>
<ol start="3"><li>Download</li><li>Install<ul><li>macOS</li><li>Linux <p>tarball</p></li></ul></li></ol>
<pre><code class="language-go">package main

func main() {
	println("hi")
}</code></pre>
<blockquote><p>Quoted text</p><p>Second para</p></blockquote>
<table><caption>Releases</caption>
<thead><tr><th>Version</th><th>Date</th></tr></thead>
<tbody><tr><td>1.26</td><td>Feb | 2026</td></tr><tr><td colspan="2">LTS</td></tr></tbody></table>
<dl><dt>GOOS</dt><dd>Target OS</dd></dl>
<p hidden>secret hidden</p><p style="display: none">also hidden</p><p aria-hidden="true">aria hidden</p>
<form><input value="search"><button>Go</button></form>
<p><a href="javascript:void(0)">js link</a> <a href="#top">anchor</a> <img src="/logo.png" alt="Logo"> <img src="x.png"></p>
<hr>
<p>Code with backtick: <code>a ` + "`" + `b</code></p>
</main>
<footer>Copyright</footer>
</body></html>`
	base, _ := url.Parse("https://example.com/start")
	doc, err := HTMLToMarkdown(src, base)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title != "Go Docs" {
		t.Fatalf("title %q", doc.Title)
	}
	md := doc.Markdown
	want := []string{
		"# Getting *started*",
		"Install Go from [the download page](https://go.dev/doc/install), then run `go version`.\nNext line.",
		"Use **modules** and **bold** text, ~~not GOPATH~~.",
		"## Steps",
		"3. Download\n4. Install\n   - macOS\n   - Linux\n     tarball",
		"```go\npackage main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n```",
		"> Quoted text\n>\n> Second para",
		"**Releases**\n\n| Version | Date |\n| --- | --- |\n| 1.26 | Feb \\| 2026 |\n| LTS |  |",
		"**GOOS**\n: Target OS",
		"js link anchor ![Logo](https://go.dev/logo.png)",
		"---",
		"Code with backtick: ``a `b``",
	}
	for _, w := range want {
		if !strings.Contains(md, w) {
			t.Errorf("missing %q in:\n%s", w, md)
		}
	}
	for _, bad := range []string{"alert", "color:red", "Home", "Blog", "Site banner", "secret", "also hidden", "aria hidden", "search", "Copyright", "javascript:", "#top"} {
		if strings.Contains(md, bad) {
			t.Errorf("%q should be dropped:\n%s", bad, md)
		}
	}
	if strings.Contains(md, "\n\n\n") {
		t.Errorf("runs of blank lines:\n%q", md)
	}
}

func TestHTMLWithoutMainUsesBodyOrSingleArticle(t *testing.T) {
	doc, _ := HTMLToMarkdown(`<body><div>intro</div><article><h3>Post</h3><p>Body</p></article><aside>ads</aside></body>`, nil)
	if doc.Markdown != "### Post\n\nBody" {
		t.Fatalf("single article: %q", doc.Markdown)
	}
	doc, _ = HTMLToMarkdown(`<body><div>intro <span>text</span></div><article>A</article><article>B</article></body>`, nil)
	if doc.Markdown != "intro text\n\nA\n\nB" {
		t.Fatalf("body: %q", doc.Markdown)
	}
	doc, _ = HTMLToMarkdown(`plain <b>fragment</b> with <a href="https://x.dev/a b">link</a>`, nil)
	if doc.Markdown != "plain **fragment** with [link](https://x.dev/a%20b)" {
		t.Fatalf("fragment: %q", doc.Markdown)
	}
}

func TestHTMLCodeFenceAndLayoutTable(t *testing.T) {
	doc, _ := HTMLToMarkdown("<pre class=\"lang-sh\">echo ```\n\nls</pre><table><tr><td>only</td></tr><tr><td>cells</td></tr></table>", nil)
	want := "````sh\necho ```\n\nls\n````\n\nonly\n\ncells"
	if doc.Markdown != want {
		t.Fatalf("got %q want %q", doc.Markdown, want)
	}
}
