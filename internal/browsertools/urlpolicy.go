package browsertools

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// CheckURL validates a URL the model asks to open and returns it in
// canonical form. Allowed are http and https, about:blank, and file URLs
// (or plain paths) that resolve inside root; javascript:, data: and every
// other scheme are refused. A bare host such as "example.com/x" gets
// https://. An empty root refuses file access entirely.
func CheckURL(raw, root string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url is required")
	}
	if len(raw) > 8192 {
		return "", errors.New("url is longer than 8192 bytes")
	}
	if strings.ContainsAny(raw, "\x00\r\n\t") {
		return "", errors.New("url contains control characters")
	}
	if strings.EqualFold(raw, "about:blank") {
		return "about:blank", nil
	}
	scheme := ""
	if i := strings.Index(raw, ":"); i > 0 {
		cand := strings.ToLower(raw[:i])
		if isScheme(cand) && !looksLikeHostPort(raw) {
			scheme = cand
		}
	}
	switch scheme {
	case "http", "https":
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("invalid url: %w", err)
		}
		if u.Host == "" || u.Hostname() == "" {
			return "", fmt.Errorf("invalid url %q: no host", raw)
		}
		u.Scheme = scheme
		return u.String(), nil
	case "file":
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("invalid url: %w", err)
		}
		if u.Host != "" && u.Host != "localhost" {
			return "", fmt.Errorf("file URLs on another host (%s) are not allowed", u.Host)
		}
		return fileURL(u.Path, root)
	case "":
		// A path to a workspace file, or a bare host.
		if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../") || strings.HasPrefix(raw, "~/") {
			p := raw
			if !filepath.IsAbs(p) && root != "" && !strings.HasPrefix(p, "~/") {
				p = filepath.Join(root, p)
			}
			return fileURL(p, root)
		}
		if root != "" {
			if st, err := os.Stat(filepath.Join(root, raw)); err == nil && !st.IsDir() {
				return fileURL(filepath.Join(root, raw), root)
			}
		}
		if strings.ContainsAny(raw, " <>\"{}|\\^`") || !strings.Contains(strings.SplitN(raw, "/", 2)[0], ".") && !strings.HasPrefix(strings.ToLower(raw), "localhost") {
			return "", fmt.Errorf("invalid url %q: give a full http(s) URL", raw)
		}
		host := strings.ToLower(strings.SplitN(strings.SplitN(raw, "/", 2)[0], ":", 2)[0])
		if host == "localhost" || strings.HasPrefix(host, "127.") || host == "0.0.0.0" {
			return CheckURL("http://"+raw, root)
		}
		return CheckURL("https://"+raw, root)
	case "javascript", "vbscript":
		return "", fmt.Errorf("%s: URLs are not allowed; use browser_eval to run script", scheme)
	default:
		return "", fmt.Errorf("the %s: scheme is not allowed (use http, https, about:blank or a file in the workspace)", scheme)
	}
}

func isScheme(s string) bool {
	if s == "" || !(s[0] >= 'a' && s[0] <= 'z') {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// looksLikeHostPort reports whether raw is host:port[/...] without a
// scheme, such as localhost:8080/app.
func looksLikeHostPort(raw string) bool {
	i := strings.Index(raw, ":")
	rest := raw[i+1:]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	return j > 0 && (j == len(rest) || rest[j] == '/' || rest[j] == '?' || rest[j] == '#')
}

// fileURL allows a local file only inside root, after resolving symlinks.
func fileURL(p, root string) (string, error) {
	if root == "" {
		return "", errors.New("file URLs are not allowed without a workspace")
	}
	if strings.HasPrefix(p, "~/") {
		return "", errors.New("file URLs must point inside the workspace")
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("file %s: %w", p, err)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		rootReal = root
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("file %s is outside the workspace", p)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(real)}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path // Windows drive paths
	}
	return u.String(), nil
}
