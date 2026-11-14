package browsertools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// agentJS is the page-side helper. It is a function expression taking a
// method name and an argument array; see agent.js.
//
//go:embed agent.js
var agentJS string

// AgentScript returns the embedded page-side helper source.
func AgentScript() string { return agentJS }

// agentExpr builds the expression that runs one helper method.
func agentExpr(method string, args ...any) (string, error) {
	if args == nil {
		args = []any{}
	}
	a, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("encode %s arguments: %w", method, err)
	}
	m, _ := json.Marshal(method)
	var b strings.Builder
	b.Grow(len(agentJS) + len(a) + len(m) + 4)
	b.WriteString(agentJS)
	b.WriteString("(")
	b.Write(m)
	b.WriteString(",")
	b.Write(a)
	b.WriteString(")")
	return b.String(), nil
}

// agentError is a failure the page-side helper reported.
type agentError struct {
	Msg        string
	Candidates []string
}

func (e *agentError) Error() string {
	if len(e.Candidates) == 0 {
		return e.Msg
	}
	return e.Msg + ":\n  " + strings.Join(e.Candidates, "\n  ")
}

// callAgent runs a helper method and decodes its result into out. A result
// of the form {"error": "..."} becomes an *agentError.
func callAgent(ctx context.Context, d Driver, out any, method string, args ...any) error {
	expr, err := agentExpr(method, args...)
	if err != nil {
		return err
	}
	raw, err := d.Evaluate(ctx, expr)
	if err != nil {
		return fmt.Errorf("page script (%s): %w", method, err)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("page script (%s) returned nothing; the page may be navigating", method)
	}
	if raw[0] == '{' {
		var e struct {
			Error      *string  `json:"error"`
			Candidates []string `json:"candidates"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != nil {
			return &agentError{Msg: *e.Error, Candidates: e.Candidates}
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("page script (%s): unexpected result: %w", method, err)
	}
	return nil
}

func isAgentError(err error) bool {
	var ae *agentError
	return errors.As(err, &ae)
}
