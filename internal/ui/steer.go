package ui

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	placeholderIdle    = "Ask anything · / for commands · @ to attach a file · ! to run a shell command"
	placeholderRunning = "Guide the agent while it works · enter steers now · ctrl+g queues for later"
	// escTwiceWindow is how quickly the second esc must follow the first
	// to interrupt a running turn.
	escTwiceWindow = 1500 * time.Millisecond
	gTip           = "✱"
	gSteer         = "↳"
)

// renderSteer shows a message the user sent while the agent was working.
func renderSteer(text string, width int) string {
	body := strings.Join(strings.Fields(text), " ")
	return "  " + sAmber.Render(gSteer+" steer ") + sText.Render(clip(body, max(10, width-12)))
}

// sessionTitle derives a short title from the first prompt: its first
// line without attachments or shell/command prefixes, at most five words
// and 28 characters, first letter capitalised.
func sessionTitle(prompt string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	var words []string
	for _, w := range strings.Fields(line) {
		if strings.HasPrefix(w, "@") || strings.HasPrefix(w, "/") || strings.HasPrefix(w, "!") || strings.Contains(w, "://") {
			continue
		}
		w = strings.TrimFunc(w, func(r rune) bool { return unicode.IsPunct(r) && r != '-' && r != '\'' })
		if w == "" {
			continue
		}
		words = append(words, w)
		if len(words) == 5 {
			break
		}
	}
	t := strings.Join(words, " ")
	for utf8.RuneCountInString(t) > 28 && len(words) > 1 {
		words = words[:len(words)-1]
		t = strings.Join(words, " ")
	}
	if r := []rune(t); utf8.RuneCountInString(t) > 28 {
		t = string(r[:27]) + "…"
	}
	if t == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(t)
	return string(unicode.ToUpper(r)) + t[n:]
}

// tips are the "Did you know" lines on the banner.
var tips = []string{
	"Type while the agent works to steer it right away; ctrl+g queues the message until the turn ends instead.",
	"Press esc twice to interrupt a running turn, then say what to do instead.",
	"ctrl+o cycles how much tool output you see: one line per call, collapsed, or everything.",
	"alt+t shows or hides the model's thinking summaries.",
	"Start a line with ! to run a shell command; its output goes with your next message.",
	"Type @ and a path to attach a file to your message.",
	"shift+tab cycles the permission mode: ask, auto-edit, plan and yolo.",
	"gta --browser gives the agent a real browser and docks a live view beside the prompt (ctrl+b).",
	"/compact summarises the conversation to free up context.",
	"/undo rolls back the agent's last file changes from a checkpoint.",
}

// tipOfTheDay picks a tip that changes every hour.
func tipOfTheDay(now time.Time) string {
	return tips[int(now.Unix()/3600)%len(tips)]
}
