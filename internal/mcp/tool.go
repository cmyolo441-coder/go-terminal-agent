package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// ToolPrefix starts the name of every MCP tool exposed to the model.
const ToolPrefix = "mcp__"

// MaxToolNameLen is the longest tool name the model APIs accept.
const MaxToolNameLen = 64

// maxDescriptionLen caps a server-supplied description sent to the model.
const maxDescriptionLen = 4096

// maxSchemaDepth bounds $ref inlining so recursive schemas terminate.
const maxSchemaDepth = 8

// Caller runs a tool on a server. Both *Client and the Manager's server
// handles (which reconnect after a crash) implement it.
type Caller interface {
	CallTool(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error)
}

// Tool adapts one MCP server tool to tools.Tool.
type Tool struct {
	server string
	info   ToolInfo
	caller Caller
	spec   llm.ToolSpec
}

var _ tools.Tool = (*Tool)(nil)

// NewTool wraps a server tool. name is the model-facing name; pass "" to use
// ToolName(server, info.Name).
func NewTool(server, name string, info ToolInfo, caller Caller) *Tool {
	if name == "" {
		name = ToolName(server, info.Name)
	}
	props, req := schemaParts(info.InputSchema)
	desc := strings.TrimSpace(info.Description)
	if desc == "" {
		desc = fmt.Sprintf("%s (from MCP server %q).", info.DisplayTitle(), server)
	}
	return &Tool{
		server: server,
		info:   info,
		caller: caller,
		spec: llm.ToolSpec{
			Name:        name,
			Description: clip(desc, maxDescriptionLen),
			Properties:  props,
			Required:    req,
		},
	}
}

// ToolName builds "mcp__<server>__<tool>", replacing characters outside
// [a-zA-Z0-9_-] with '_'. Names longer than 64 characters are cut and given a
// hash suffix so distinct tools keep distinct names.
func ToolName(server, tool string) string {
	full := ToolPrefix + sanitize(server) + "__" + sanitize(tool)
	if len(full) <= MaxToolNameLen {
		return full
	}
	h := fnv.New32a()
	h.Write([]byte(server + "\x00" + tool))
	suffix := fmt.Sprintf("_%08x", h.Sum32())
	return full[:MaxToolNameLen-len(suffix)] + suffix
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// Server is the MCP server name the tool belongs to.
func (t *Tool) Server() string { return t.server }

// Info is the server's description of the tool.
func (t *Tool) Info() ToolInfo { return t.info }

// Spec implements tools.Tool.
func (t *Tool) Spec() llm.ToolSpec { return t.spec }

// Kind implements tools.Tool. MCP tools can do anything, so every call goes
// through the permission gate like a shell command.
func (t *Tool) Kind() tools.Kind { return tools.KindExec }

// Prepare implements tools.Tool: the input must be a single JSON object that
// carries every required property.
func (t *Tool) Prepare(env *tools.Env, input json.RawMessage) (*tools.Call, error) {
	args := bytes.TrimSpace(input)
	if len(args) == 0 {
		args = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, errors.New("invalid tool input: must be a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid tool input: trailing data after the JSON object")
	}
	for _, r := range t.spec.Required {
		if _, ok := obj[r]; !ok {
			return nil, fmt.Errorf("invalid tool input: %q is required", r)
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, args); err != nil {
		return nil, fmt.Errorf("invalid tool input: %w", err)
	}
	payload := json.RawMessage(compact.Bytes())

	limit := 0
	if env != nil {
		limit = env.OutputLimit
	}
	preview := "{}"
	if len(obj) > 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, payload, "", "  ") == nil {
			preview = pretty.String()
		}
	}
	summary := fmt.Sprintf("%s (MCP %s)", t.info.DisplayTitle(), t.server)
	name, toolName, caller := t.spec.Name, t.info.Name, t.caller
	run := func(ctx context.Context) (string, error) {
		res, err := caller.CallTool(ctx, toolName, payload)
		if err != nil {
			return "", err
		}
		out := truncate(res.Text(), limit)
		if res.IsError {
			if strings.TrimSpace(out) == "" {
				out = "(no details)"
			}
			return out, fmt.Errorf("MCP tool %s reported an error", name)
		}
		if strings.TrimSpace(out) == "" {
			out = "(tool returned no output)"
		}
		return out, nil
	}
	return tools.NewCall(name, tools.KindExec, summary, clip(preview, 4000), name, run), nil
}

// schemaParts extracts properties and required from a JSON Schema object,
// inlining local $refs so the model sees a self-contained schema.
func schemaParts(raw json.RawMessage) (map[string]any, []string) {
	props := map[string]any{}
	var schema map[string]any
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &schema) != nil || schema == nil {
		return props, nil
	}
	defs := map[string]any{}
	for _, key := range []string{"definitions", "$defs"} {
		if d, ok := schema[key].(map[string]any); ok {
			for k, v := range d {
				defs["#/"+key+"/"+k] = v
			}
		}
	}
	if p, ok := schema["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = inlineRefs(v, defs, 0)
		}
	}
	var req []string
	if r, ok := schema["required"].([]any); ok {
		for _, v := range r {
			if s, ok := v.(string); ok {
				req = append(req, s)
			}
		}
	}
	sort.Strings(req)
	return props, req
}

// inlineRefs replaces {"$ref": "#/$defs/X"} with the definition, up to a
// fixed depth; deeper or unknown references become an unconstrained schema.
func inlineRefs(node any, defs map[string]any, depth int) any {
	switch n := node.(type) {
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok {
			def, found := defs[ref]
			if !found || depth >= maxSchemaDepth {
				out := map[string]any{}
				if d, ok := n["description"]; ok {
					out["description"] = d
				}
				return out
			}
			return inlineRefs(def, defs, depth+1)
		}
		out := make(map[string]any, len(n))
		for k, v := range n {
			if k == "$defs" || k == "definitions" {
				continue
			}
			out[k] = inlineRefs(v, defs, depth)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = inlineRefs(v, defs, depth)
		}
		return out
	default:
		return node
	}
}

// truncate keeps the head and tail of long output.
func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head := limit * 2 / 3
	tail := limit - head
	for head > 0 && (s[head]&0xC0) == 0x80 {
		head--
	}
	start := len(s) - tail
	for start < len(s) && (s[start]&0xC0) == 0x80 {
		start++
	}
	return s[:head] + fmt.Sprintf("\n\n… [%d characters omitted] …\n\n", start-head) + s[start:]
}
