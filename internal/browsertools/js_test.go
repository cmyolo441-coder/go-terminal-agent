package browsertools

import (
	"encoding/json"
	"strings"
	"testing"
)

// checkBalanced verifies that brackets in JavaScript source balance,
// skipping strings, template literals and comments. Regex literals in the
// helper are themselves balanced, so they need no special handling.
func checkBalanced(src string) error {
	var stack []byte
	pairs := map[byte]byte{')': '(', ']': '[', '}': '{'}
	line := 1
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\n':
			line++
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			line++
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return errAt(line, "unterminated comment")
			}
			line += strings.Count(src[i:i+2+end], "\n")
			i += end + 3
		case c == '"' || c == '\'' || c == '`':
			j := i + 1
			for ; j < len(src) && src[j] != c; j++ {
				if src[j] == '\\' {
					j++
				} else if src[j] == '\n' && c != '`' {
					return errAt(line, "newline in string")
				}
			}
			if j >= len(src) {
				return errAt(line, "unterminated string")
			}
			i = j
		case c == '(' || c == '[' || c == '{':
			stack = append(stack, c)
		case c == ')' || c == ']' || c == '}':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[c] {
				return errAt(line, "unbalanced "+string(c))
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) != 0 {
		return errAt(line, "unclosed "+string(stack))
	}
	return nil
}

type jsErr string

func (e jsErr) Error() string { return string(e) }

func errAt(line int, msg string) error {
	return jsErr("line " + itoa(line) + ": " + msg)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestAgentScriptWellFormed(t *testing.T) {
	src := AgentScript()
	if len(src) < 5000 {
		t.Fatalf("agent script is only %d bytes", len(src))
	}
	if err := checkBalanced(src); err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSpace(src)
	// Strip the leading comment block.
	for strings.HasPrefix(body, "//") {
		body = strings.TrimSpace(body[strings.IndexByte(body, '\n')+1:])
	}
	if !strings.HasPrefix(body, "(function (method, args) {") || !strings.HasSuffix(body, "})") {
		t.Fatal("agent script must be a single function expression")
	}
	for _, m := range []string{"snapshot:", "locate:", "hit:", "scrollIntoView:", "scrollInfo:", "state:", "focus:", "selectAll:", "clear:", "select:", "findOption:", "extract:", "probe:"} {
		if !strings.Contains(src, m) {
			t.Errorf("agent script lacks method %s", m)
		}
	}
	for _, want := range []string{"shadowRoot", "contentDocument", "elementFromPoint", "aria-labelledby", "data-gta-ref"} {
		if !strings.Contains(src, want) {
			t.Errorf("agent script lacks %s handling", want)
		}
	}
}

func TestCheckBalancedCatchesErrors(t *testing.T) {
	for _, bad := range []string{"(function(){", "})", "f(\"a)", "[1, 2)", "/* x", "a = 'x\ny'"} {
		if checkBalanced(bad) == nil {
			t.Errorf("checkBalanced(%q) should fail", bad)
		}
	}
	for _, good := range []string{"f('(', \"]\", `{`)", "// (\n[]", "/* { */ ()", "x = /[a-z]+/g"} {
		if err := checkBalanced(good); err != nil {
			t.Errorf("checkBalanced(%q): %v", good, err)
		}
	}
}

func TestAgentExpr(t *testing.T) {
	expr, err := agentExpr("locate", map[string]string{"text": `a"b</script>`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(expr, agentJS+"(") || !strings.HasSuffix(expr, ")") {
		t.Fatal("expression must call the helper")
	}
	tail := strings.TrimSuffix(strings.TrimPrefix(expr, agentJS+"("), ")")
	var parts []json.RawMessage
	if err := json.Unmarshal([]byte("["+tail+"]"), &parts); err != nil || len(parts) != 2 {
		t.Fatalf("arguments are not JSON: %q (%v)", tail, err)
	}
	if _, err := agentExpr("x", func() {}); err == nil {
		t.Fatal("unencodable arguments should fail")
	}
	noArgs, _ := agentExpr("scrollInfo")
	if !strings.HasSuffix(noArgs, `("scrollInfo",[])`) {
		t.Fatalf("no-arg call = %q", noArgs[len(noArgs)-30:])
	}
}
