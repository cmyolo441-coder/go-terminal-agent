package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// TodoWrite replaces the agent's visible task list.
type TodoWrite struct{}

type todoInput struct {
	Todos []Todo `json:"todos"`
}

func (TodoWrite) Kind() Kind { return KindRead }

func (TodoWrite) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: "todo_write",
		Description: "Replace the task list shown to the user. Use it for work with three or more steps: " +
			"keep exactly one item in_progress, and mark items completed as soon as they are done.",
		Properties: map[string]any{
			"todos": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": map[string]any{"type": "string"},
						"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
					},
					"required": []string{"content", "status"},
				},
			},
		},
		Required: []string{"todos"},
	}
}

func (TodoWrite) Prepare(env *Env, raw json.RawMessage) (*Call, error) {
	var in todoInput
	if err := decode(raw, &in); err != nil {
		return nil, err
	}
	for i, t := range in.Todos {
		if strings.TrimSpace(t.Content) == "" {
			return nil, fmt.Errorf("invalid tool input: todo %d has empty content", i+1)
		}
		switch t.Status {
		case "pending", "in_progress", "completed":
		default:
			return nil, fmt.Errorf("invalid tool input: todo %d has status %q", i+1, t.Status)
		}
	}
	todos := in.Todos
	return &Call{Tool: "todo_write", Kind: KindRead, Summary: "Update task list", run: func(ctx context.Context) (string, error) {
		env.setTodos(todos)
		done := 0
		for _, t := range todos {
			if t.Status == "completed" {
				done++
			}
		}
		return fmt.Sprintf("task list updated (%d/%d completed)", done, len(todos)), nil
	}}, nil
}
