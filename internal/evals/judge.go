package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/cmyolo441-coder/go-terminal-agent/internal/llm"
)

// JudgeRequest is what an llm_judge grader asks the judge to assess.
type JudgeRequest struct {
	Rubric     string
	Prompt     string
	Output     string
	Transcript *Transcript
	Workspace  string
	// Scale is the maximum score the judge should use.
	Scale float64
}

// JudgeFunc assesses a run against a rubric and returns the judge's raw
// reply, which ParseJudgeScore turns into a score.
type JudgeFunc func(ctx context.Context, req JudgeRequest) (string, error)

// ErrNoScore is returned when no score can be found in a judge reply.
var ErrNoScore = errors.New("evals: no score found in the judge reply")

var (
	// A labelled score: "Score: 8/10", "**Final score**: 7.5 out of 10",
	// "rating = 80%", "Score - 4".
	labelledScoreRe = regexp.MustCompile(`(?i)\b(?:final\s+score|overall\s+score|total\s+score|score|rating|grade)\b(?:[\s*_:=–—>|"']|-\s)*(?:is\s+|of\s+)?[\s*_"']*(-?\d+(?:\.\d+)?)[\s*_"']*(?:(?:/|out\s+of|of)\s*(\d+(?:\.\d+)?)|(%))?`)
	// An unlabelled fraction: "8/10", "8 out of 10".
	fractionRe = regexp.MustCompile(`(?i)(?:^|[^\w.])(-?\d+(?:\.\d+)?)\s*(?:/|out\s+of)\s*(\d+(?:\.\d+)?)\b`)
	// A verdict: "Verdict: PASS".
	verdictRe = regexp.MustCompile(`(?i)\bverdict\b[\s*_:=\-–—>"']*\b(pass|fail|passed|failed|yes|no)\b`)
	bareRe    = regexp.MustCompile(`^[\s*_"'` + "`" + `]*(-?\d+(?:\.\d+)?)\s*(%)?[\s*_"'.` + "`" + `]*$`)
)

// ParseJudgeScore extracts a score from a judge reply and normalises it to
// [0, 1]. It accepts, in order of preference: a JSON object with a "score"
// field (optionally "max"/"out_of"/"scale"), possibly inside a code fence;
// the last labelled score ("Score: 8/10", "**Rating:** 4.5 out of 5",
// "score = 85%"); the last bare fraction ("7/10"); a pass/fail verdict;
// or a reply that is just a number. Scores without an explicit maximum are
// divided by scale, except values already in [0, 1] when scale is 1.
func ParseJudgeScore(reply string, scale float64) (float64, error) {
	if scale <= 0 {
		scale = 10
	}
	if v, ok := jsonScore(reply, scale); ok {
		return v, checkUnit(v, reply)
	}
	if ms := labelledScoreRe.FindAllStringSubmatch(reply, -1); len(ms) > 0 {
		m := ms[len(ms)-1]
		v, err := normaliseScore(m[1], m[2], m[3] != "", scale)
		if err != nil {
			return 0, err
		}
		return v, checkUnit(v, reply)
	}
	if ms := fractionRe.FindAllStringSubmatch(reply, -1); len(ms) > 0 {
		m := ms[len(ms)-1]
		v, err := normaliseScore(m[1], m[2], false, scale)
		if err != nil {
			return 0, err
		}
		return v, checkUnit(v, reply)
	}
	if m := verdictRe.FindStringSubmatch(reply); m != nil {
		switch strings.ToLower(m[1]) {
		case "pass", "passed", "yes":
			return 1, nil
		default:
			return 0, nil
		}
	}
	if m := bareRe.FindStringSubmatch(strings.TrimSpace(reply)); m != nil {
		v, err := normaliseScore(m[1], "", m[2] != "", scale)
		if err != nil {
			return 0, err
		}
		return v, checkUnit(v, reply)
	}
	return 0, fmt.Errorf("%w: %q", ErrNoScore, truncateRunes(strings.TrimSpace(reply), 200))
}

func checkUnit(v float64, reply string) error {
	if v < 0 || v > 1 || math.IsNaN(v) {
		return fmt.Errorf("evals: judge score %.4g is outside the scale in %q", v, truncateRunes(strings.TrimSpace(reply), 200))
	}
	return nil
}

func normaliseScore(num, den string, percent bool, scale float64) (float64, error) {
	n, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("evals: bad judge score %q", num)
	}
	switch {
	case percent:
		return n / 100, nil
	case den != "":
		d, err := strconv.ParseFloat(den, 64)
		if err != nil || d <= 0 {
			return 0, fmt.Errorf("evals: bad judge score maximum %q", den)
		}
		return n / d, nil
	default:
		return n / scale, nil
	}
}

// jsonScore looks for {"score": ...} (or "rating") in the reply. A string
// score such as "8/10" is parsed with the text rules.
func jsonScore(reply string, scale float64) (float64, bool) {
	raw, ok := ExtractJSON(reply)
	if !ok {
		return 0, false
	}
	v, err := decodeJSONValue(raw)
	if err != nil {
		return 0, false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return 0, false
	}
	var score any
	var den float64
	for _, k := range sortedKeys(obj) {
		switch strings.ToLower(k) {
		case "score", "rating":
			score = obj[k]
		case "max", "out_of", "scale", "max_score":
			if n, ok := obj[k].(json.Number); ok {
				if f, err := n.Float64(); err == nil && f > 0 {
					den = f
				}
			}
		}
	}
	switch x := score.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return 0, false
		}
		if den > 0 {
			return f / den, true
		}
		return f / scale, true
	case string:
		if den > 0 {
			scale = den
		}
		f, err := ParseJudgeScore(x, scale)
		return f, err == nil
	}
	return 0, false
}

// judgeSystem instructs a model acting as judge.
const judgeSystem = `You are a strict, impartial evaluator of an AI coding agent's work.
Assess the agent's final answer against the rubric. Be specific and brief.
End your reply with a line of the form "Score: N/M", where M is the maximum score you were given.`

// ProviderJudge returns a JudgeFunc that asks a model through provider.
func ProviderJudge(provider llm.Provider, model string, maxTokens int64) JudgeFunc {
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	return func(ctx context.Context, req JudgeRequest) (string, error) {
		var b strings.Builder
		fmt.Fprintf(&b, "## Task given to the agent\n%s\n\n## Rubric\n%s\n\n", req.Prompt, req.Rubric)
		if req.Transcript != nil && len(req.Transcript.ToolCalls) > 0 {
			b.WriteString("## Tool calls made\n")
			for i, c := range req.Transcript.ToolCalls {
				if i == 50 {
					fmt.Fprintf(&b, "… %d more\n", len(req.Transcript.ToolCalls)-50)
					break
				}
				fmt.Fprintf(&b, "- %s %s\n", c.Name, truncateRunes(string(c.Input), 200))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "## Agent's final answer\n%s\n\nScore the answer from 0 to %g.", req.Output, req.Scale)
		resp, err := provider.Stream(ctx, llm.Request{
			Model:     model,
			System:    judgeSystem,
			Messages:  []llm.Message{llm.UserText(b.String())},
			MaxTokens: maxTokens,
		}, func(llm.Event) {})
		if err != nil {
			return "", err
		}
		if resp == nil {
			return "", errors.New("judge returned no response")
		}
		return resp.Message.Text(), nil
	}
}
