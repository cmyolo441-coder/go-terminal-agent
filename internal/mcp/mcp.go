// Package mcp is a minimal Model Context Protocol client for stdio servers.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/config"
	"github.com/cmyolo441-coder/go-terminal-agent/internal/tools"
)

// Client is a connection to one MCP server process.
type Client struct {
	Name  string
	Tools []ToolInfo

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResponse
	closed  chan struct{}
}

// ToolInfo is a tool advertised by a server.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type rpcResponse struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Connect starts the server, performs the handshake and lists its tools.
func Connect(ctx context.Context, name string, cfg config.MCPServer, cwd string) (*Client, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("no command configured (only stdio servers are supported)")
	}
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{Name: name, cmd: cmd, stdin: stdin, pending: map[int]chan rpcResponse{}, closed: make(chan struct{})}
	go c.readLoop(stdout)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": config.AppName, "version": config.Version},
	})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	_ = c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	res, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var list struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		c.Close()
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	c.Tools = list.Tools
	return c, nil
}

func (c *Client) readLoop(r io.Reader) {
	defer close(c.closed)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32<<20)
	for sc.Scan() {
		var resp rpcResponse
		if json.Unmarshal(sc.Bytes(), &resp) != nil || resp.ID == nil {
			continue // notification, server request or noise
		}
		c.mu.Lock()
		ch := c.pending[*resp.ID]
		delete(c.pending, *resp.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s (code %d)", resp.Error.Message, resp.Error.Code)
		}
		return resp.Result, nil
	case <-c.closed:
		return nil, fmt.Errorf("server exited")
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// CallTool invokes a tool and flattens its content into a tool result.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) tools.Result {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	res, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return tools.Errorf("MCP call failed: %v", err)
	}
	var out struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return tools.Errorf("bad MCP result: %v", err)
	}
	r := tools.Result{IsError: out.IsError}
	var parts []string
	for _, c := range out.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image":
			r.Images = append(r.Images, tools.Image{MediaType: c.MimeType, Data: c.Data})
		default:
			parts = append(parts, fmt.Sprintf("[%s content omitted]", c.Type))
		}
	}
	r.Text = strings.Join(parts, "\n")
	if r.Text == "" && len(r.Images) == 0 {
		r.Text = "(empty result)"
	}
	if len(r.Text) > 100000 {
		r.Text = r.Text[:100000] + "\n[result truncated]"
	}
	return r
}

// Close stops the server process.
func (c *Client) Close() {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
}

var reBadName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// ToolName builds the namespaced tool name the model sees.
func ToolName(server, tool string) string {
	return "mcp__" + reBadName.ReplaceAllString(server, "_") + "__" + reBadName.ReplaceAllString(tool, "_")
}

// Register adds the client's tools to a registry.
func (c *Client) Register(reg *tools.Registry) {
	for _, t := range c.Tools {
		t := t
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		_ = json.Unmarshal(t.InputSchema, &schema)
		if schema.Properties == nil {
			schema.Properties = map[string]any{}
		}
		reg.Add(&tools.Tool{
			Name:        ToolName(c.Name, t.Name),
			Description: t.Description,
			Properties:  schema.Properties,
			Required:    schema.Required,
			Summary: func(in json.RawMessage, _ *tools.Env) string {
				return string(in)
			},
			Run: func(ctx context.Context, in json.RawMessage, _ *tools.Env) tools.Result {
				return c.CallTool(ctx, t.Name, in)
			},
		})
	}
}
