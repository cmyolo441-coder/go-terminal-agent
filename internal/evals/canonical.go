package evals

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// CanonicalOptions controls how a request is reduced to the canonical tree
// that is hashed for cassette matching.
type CanonicalOptions struct {
	// IgnoreFields are dotted paths into the canonical tree that are removed
	// before hashing. A "*" segment matches any object key or array index,
	// for example "max_tokens", "system" or "messages.*.blocks.*.is_error".
	IgnoreFields []string
	// IncludeNative keeps Message.Provider and Message.Native in the tree.
	// They are provider-specific replay blobs (thinking signatures and the
	// like) that differ between otherwise identical conversations, so they
	// are dropped by default.
	IncludeNative bool
	// KeepToolIDs keeps tool-use ids verbatim. By default they are renamed
	// in order of first appearance (tool_1, tool_2, ...) because providers
	// mint fresh random ids on every call.
	KeepToolIDs bool
	// KeepToolOrder keeps the request's tool list in its original order.
	// By default tools are sorted by name, since registration order is an
	// implementation detail of the agent.
	KeepToolOrder bool
}

// canonicalRequest converts a request into a generic JSON tree (maps,
// slices, strings, json.Number, bools, nil) after applying the transform to
// every string. The tree deliberately excludes fields the hash must not see
// (see CanonicalOptions); IgnoreFields are applied later by hashTree so a
// stored tree can be re-hashed under different options.
func canonicalRequest(req llm.Request, opts CanonicalOptions, transform func(string) string) map[string]any {
	if transform == nil {
		transform = func(s string) string { return s }
	}
	ids := map[string]string{}
	toolID := func(id string) string {
		if id == "" {
			return ""
		}
		if opts.KeepToolIDs {
			return transform(id)
		}
		if n, ok := ids[id]; ok {
			return n
		}
		n := "tool_" + strconv.Itoa(len(ids)+1)
		ids[id] = n
		return n
	}

	tree := map[string]any{
		"model":         transform(req.Model),
		"system":        transform(req.System),
		"max_tokens":    json.Number(strconv.FormatInt(req.MaxTokens, 10)),
		"effort":        transform(req.Effort),
		"show_thinking": req.ShowThinking,
	}

	tools := make([]llm.ToolSpec, len(req.Tools))
	copy(tools, req.Tools)
	if !opts.KeepToolOrder {
		sort.SliceStable(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	}
	toolList := make([]any, 0, len(tools))
	for _, t := range tools {
		required := make([]any, 0, len(t.Required))
		rs := append([]string(nil), t.Required...)
		sort.Strings(rs)
		for _, r := range rs {
			required = append(required, transform(r))
		}
		props, _ := normalizeAny(t.Properties, transform)
		if props == nil {
			props = map[string]any{}
		}
		toolList = append(toolList, map[string]any{
			"name":        transform(t.Name),
			"description": transform(t.Description),
			"properties":  props,
			"required":    required,
		})
	}
	tree["tools"] = toolList

	msgs := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		blocks := make([]any, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			blocks = append(blocks, canonicalBlock(b, toolID, transform))
		}
		cm := map[string]any{
			"role":   string(m.Role),
			"blocks": blocks,
		}
		if opts.IncludeNative {
			cm["provider"] = transform(m.Provider)
			if len(m.Native) > 0 {
				cm["native"] = normalizeRawJSON(m.Native, transform)
			}
		}
		msgs = append(msgs, cm)
	}
	tree["messages"] = msgs
	return tree
}

func canonicalBlock(b llm.Block, toolID func(string) string, transform func(string) string) map[string]any {
	cb := map[string]any{"type": string(b.Type)}
	if b.Text != "" {
		cb["text"] = transform(b.Text)
	}
	if b.ToolUseID != "" {
		cb["tool_use_id"] = toolID(b.ToolUseID)
	}
	if b.ToolName != "" {
		cb["tool_name"] = transform(b.ToolName)
	}
	if len(b.Input) > 0 {
		cb["input"] = normalizeRawJSON(b.Input, transform)
	}
	if b.IsError {
		cb["is_error"] = true
	}
	if b.Image != nil {
		cb["image"] = canonicalImage(*b.Image, transform)
	}
	if len(b.Images) > 0 {
		imgs := make([]any, 0, len(b.Images))
		for _, img := range b.Images {
			imgs = append(imgs, canonicalImage(img, transform))
		}
		cb["images"] = imgs
	}
	return cb
}

// canonicalImage identifies an image by content rather than by its bytes,
// which keeps cassettes small and diffs readable.
func canonicalImage(img llm.Image, transform func(string) string) map[string]any {
	out := map[string]any{}
	if img.MediaType != "" {
		out["media_type"] = img.MediaType
	}
	if img.Data != "" {
		sum := sha256.Sum256([]byte(img.Data))
		out["data_sha256"] = hex.EncodeToString(sum[:])
	}
	if img.Path != "" {
		out["path"] = transform(img.Path)
	}
	return out
}

// normalizeRawJSON parses raw JSON into a canonical tree. Invalid JSON is
// kept as a (transformed) string so the hash still reflects it.
func normalizeRawJSON(raw json.RawMessage, transform func(string) string) any {
	v, err := decodeJSONValue(raw)
	if err != nil {
		return map[string]any{"$invalid_json": transform(string(raw))}
	}
	out, _ := normalizeAny(v, transform)
	return out
}

// decodeJSONValue decodes exactly one JSON value, preserving number text.
func decodeJSONValue(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}

// normalizeAny converts arbitrary Go values (as produced by encoding/json or
// handed in by callers, e.g. ToolSpec.Properties) into a canonical tree.
// Numbers are normalised so 1, 1.0 and 1e0 hash identically.
func normalizeAny(v any, transform func(string) string) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		return transform(x), nil
	case bool:
		return x, nil
	case json.Number:
		return normalizeNumber(x), nil
	case float64:
		return normalizeNumber(json.Number(strconv.FormatFloat(x, 'g', -1, 64))), nil
	case float32:
		return normalizeNumber(json.Number(strconv.FormatFloat(float64(x), 'g', -1, 32))), nil
	case int:
		return json.Number(strconv.Itoa(x)), nil
	case int64:
		return json.Number(strconv.FormatInt(x, 10)), nil
	case int32:
		return json.Number(strconv.FormatInt(int64(x), 10)), nil
	case uint64:
		return json.Number(strconv.FormatUint(x, 10)), nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			n, err := normalizeAny(e, transform)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			n, err := normalizeAny(e, transform)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = transform(e)
		}
		return out, nil
	default:
		// Anything else (typed maps, structs) goes through encoding/json.
		raw, err := json.Marshal(x)
		if err != nil {
			return nil, err
		}
		dv, err := decodeJSONValue(raw)
		if err != nil {
			return nil, err
		}
		return normalizeAny(dv, transform)
	}
}

// normalizeNumber renders integers exactly and other numbers in shortest
// float form.
func normalizeNumber(n json.Number) json.Number {
	s := string(n)
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return json.Number(strconv.FormatInt(i, 10))
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return n
	}
	if f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return json.Number(strconv.FormatInt(int64(f), 10))
	}
	return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
}

// hashTree removes ignored fields from a deep copy of the tree and returns
// the hex SHA-256 of its canonical JSON encoding (encoding/json sorts map
// keys, so the encoding is deterministic).
func hashTree(tree any, ignore []string) (string, error) {
	t := cloneTree(tree)
	for _, p := range ignore {
		if p = strings.TrimSpace(p); p != "" {
			t = deletePath(t, strings.Split(p, "."))
		}
	}
	raw, err := canonicalJSON(t)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalJSON encodes without HTML escaping so hashes do not depend on
// encoder settings.
func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// cloneTree deep-copies a canonical tree.
func cloneTree(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = cloneTree(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneTree(e)
		}
		return out
	default:
		return v
	}
}

// deletePath removes every node matching the dotted pattern. Array
// elements matched by the final segment are removed from the array.
func deletePath(v any, segs []string) any {
	if len(segs) == 0 {
		return v
	}
	seg, rest := segs[0], segs[1:]
	switch x := v.(type) {
	case map[string]any:
		for k := range x {
			if seg != "*" && seg != k {
				continue
			}
			if len(rest) == 0 {
				delete(x, k)
			} else {
				x[k] = deletePath(x[k], rest)
			}
		}
		return x
	case []any:
		if len(rest) == 0 {
			out := x[:0:0]
			for i, e := range x {
				if seg == "*" || seg == strconv.Itoa(i) {
					continue
				}
				out = append(out, e)
			}
			return out
		}
		for i := range x {
			if seg == "*" || seg == strconv.Itoa(i) {
				x[i] = deletePath(x[i], rest)
			}
		}
		return x
	default:
		return v
	}
}

// RequestHash returns the canonical hash of a request under the given
// options, with no secret scrubbing or path substitution. It is exposed for
// tooling (for example to look a request up in a cassette by hand).
func RequestHash(req llm.Request, opts CanonicalOptions) (string, error) {
	return hashTree(canonicalRequest(req, opts, nil), opts.IgnoreFields)
}

// transformJSONStrings applies fn to every string inside a raw JSON value,
// including object keys' values but not keys. Invalid JSON is transformed
// as plain text. The output is compact JSON.
func transformJSONStrings(raw json.RawMessage, fn func(string) string) json.RawMessage {
	if len(raw) == 0 || fn == nil {
		return raw
	}
	v, err := decodeJSONValue(raw)
	if err != nil {
		return json.RawMessage(fn(string(raw)))
	}
	v = mapStrings(v, fn)
	out, err := canonicalJSON(v)
	if err != nil {
		return raw
	}
	return out
}

func mapStrings(v any, fn func(string) string) any {
	switch x := v.(type) {
	case string:
		return fn(x)
	case map[string]any:
		for k, e := range x {
			x[k] = mapStrings(e, fn)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = mapStrings(e, fn)
		}
		return x
	default:
		return v
	}
}

// shortHash is a printable prefix of a hash for diagnostics.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
