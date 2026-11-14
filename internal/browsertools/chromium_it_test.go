package browsertools

import (
	"context"
	"encoding/json"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// findChromium returns a local Chromium or Chrome binary, or "".
func findChromium() string {
	for _, p := range []string{"/opt/pw-browsers/chromium-1243/chrome-linux/chrome", "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	for _, n := range []string{"google-chrome", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

const itPage = `<!doctype html><html><head><title>IT Page</title></head><body>
<nav aria-label="Primary"><a href="/home">Home</a></nav>
<main>
<h1>Sign in</h1>
<form>
<label for="email">Email</label><input id="email" type="email" required>
<label>Password <input type="password" value="hunter2"></label>
<label class="cb"><input type="checkbox" style="opacity:0;position:absolute"> Remember me</label>
<select id="country"><option value="us">United States</option><option value="de">Germany</option></select>
<button type="submit" disabled>Submit</button>
<div role="button" aria-expanded="false" tabindex="0">Menu</div>
</form>
<div style="display:none"><button>Hidden</button></div>
<div id="host"></div>
<iframe srcdoc="<button>Frame button</button>" style="width:300px;height:80px"></iframe>
<div style="height:2000px"></div>
<button id="low">Far below</button>
<div id="ov" style="position:fixed;left:0;top:0;width:120px;height:120px;background:red">Cookies</div>
</main>
<pre id="out"></pre>
<script>
const h = document.getElementById('host').attachShadow({mode: 'open'});
h.innerHTML = '<button>Shadow btn</button><slot></slot>';
document.getElementById('host').innerHTML = '<a href="#s">Slotted link</a>';
const AG = __AGENT__;
window.addEventListener('load', () => setTimeout(() => {
  const r = {};
  r.snap = AG("snapshot", [{}]);
  const email = r.snap.nodes.find(n => n.name === "Email").ref;
  const combo = r.snap.nodes.find(n => n.role === "combobox").ref;
  r.ambiguous = AG("locate", [{text: "in"}]);
  r.select = AG("select", [combo, ["germany"], "auto"]);
  r.badSelect = AG("select", [combo, ["france"], "auto"]);
  r.overlay = AG("hit", [email, 10, 10]);
  const li = AG("locate", [{ref: email}]);
  r.hitOK = AG("hit", [email, li.vis[0] + li.vis[2] / 2, li.vis[1] + li.vis[3] / 2]);
  r.badSelector = AG("locate", [{selector: "[[["}]);
  r.probe = AG("probe", ["text", "frame button"]);
  r.far = AG("scrollIntoView", [AG("locate", [{text: "Far below"}]).ref]);
  document.getElementById('out').textContent = JSON.stringify(r);
}, 200));
</script></body></html>`

// TestAgentScriptInChromium runs the embedded page script in a real
// headless browser. It needs no browser engine package, only a Chromium
// binary, and runs only with GTA_BROWSER_IT=1.
func TestAgentScriptInChromium(t *testing.T) {
	if os.Getenv("GTA_BROWSER_IT") != "1" {
		t.Skip("set GTA_BROWSER_IT=1 to run the Chromium integration test")
	}
	bin := findChromium()
	if bin == "" {
		t.Skip("no Chromium found")
	}
	dir := t.TempDir()
	page := filepath.Join(dir, "page.html")
	if err := os.WriteFile(page, []byte(strings.Replace(itPage, "__AGENT__", AgentScript(), 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--headless=new", "--no-sandbox", "--disable-gpu", "--user-data-dir="+filepath.Join(dir, "profile"),
		"--virtual-time-budget=3000", "--window-size=1280,720", "--dump-dom", "file://"+page)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("chromium: %v", err)
	}
	m := regexp.MustCompile(`(?s)<pre id="out">(.*?)</pre>`).FindSubmatch(out)
	if m == nil || len(m[1]) == 0 {
		t.Fatalf("no result in DOM:\n%s", out)
	}
	var r struct {
		Snap        Snapshot       `json:"snap"`
		Ambiguous   map[string]any `json:"ambiguous"`
		Select      map[string]any `json:"select"`
		BadSelect   map[string]any `json:"badSelect"`
		Overlay     map[string]any `json:"overlay"`
		HitOK       map[string]any `json:"hitOK"`
		BadSelector map[string]any `json:"badSelector"`
		Probe       map[string]any `json:"probe"`
		Far         element        `json:"far"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(string(m[1]))), &r); err != nil {
		t.Fatal(err)
	}
	outline := r.Snap.Render(0)
	for _, want := range []string{
		`navigation "Primary"`, `link "Home" -> /home`, `h1 "Sign in"`,
		`textbox "Email" (required) type=email value=""`, `textbox "Password" type=password value="•••••••"`,
		`checkbox "Remember me" (unchecked)`, `combobox value="United States" options=[United States | Germany]`,
		`button "Submit" (disabled)`, `button "Menu" (collapsed)`, `button "Shadow btn"`, `link "Slotted link"`,
		`button "Frame button"`, `button "Far below" (offscreen)`,
	} {
		if !strings.Contains(outline, want) {
			t.Errorf("outline lacks %q:\n%s", want, outline)
		}
	}
	if strings.Contains(outline, "Hidden") {
		t.Error("display:none content in the outline")
	}
	if r.Ambiguous["error"] == nil {
		t.Errorf("ambiguous text match not refused: %v", r.Ambiguous)
	}
	if sel, _ := r.Select["selected"].([]any); len(sel) != 1 || sel[0] != "Germany" {
		t.Errorf("select = %v", r.Select)
	}
	if e, _ := r.BadSelect["error"].(string); !strings.Contains(e, "options are") {
		t.Errorf("bad select = %v", r.BadSelect)
	}
	if r.Overlay["ok"] != false || !strings.Contains(r.Overlay["blocker"].(string), "div#ov") {
		t.Errorf("overlay = %v", r.Overlay)
	}
	if r.HitOK["ok"] != true {
		t.Errorf("hit = %v", r.HitOK)
	}
	if e, _ := r.BadSelector["error"].(string); !strings.Contains(e, "invalid CSS selector") {
		t.Errorf("bad selector = %v", r.BadSelector)
	}
	if r.Probe["ok"] != true {
		t.Errorf("probe into iframe = %v", r.Probe)
	}
	if !r.Far.inView() {
		t.Errorf("scrollIntoView left the element out of view: %+v", r.Far)
	}
}
