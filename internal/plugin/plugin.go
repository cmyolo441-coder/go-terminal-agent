package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Source says where a plugin was found.
type Source string

const (
	// SourceUser is the user's config directory.
	SourceUser Source = "user"
	// SourceProject is the project's .gta/plugins directory.
	SourceProject Source = "project"
)

// Plugin is a loaded, validated plugin.
type Plugin struct {
	Manifest Manifest
	// Dir is the plugin directory (absolute).
	Dir    string
	Source Source
	// Tools and Commands are what the plugin adds.
	Tools    []*Tool
	Commands []*Command
}

// Name is the plugin's name.
func (p *Plugin) Name() string { return p.Manifest.Name }

// LoadOptions describe the host a plugin is loaded into.
type LoadOptions struct {
	Source Source
	// Workspace is the project root: the working directory of plugin
	// processes and the value of ${WORKSPACE} and $GTA_WORKSPACE.
	Workspace string
	// GTAVersion is the running GTA version, checked against
	// min_gta_version.
	GTAVersion string
	// Environ returns the parent environment; nil means os.Environ.
	Environ func() []string
}

// LoadDir loads and validates the plugin in dir. Every executable must
// exist, be executable and, when it ships inside the plugin, stay inside
// the plugin directory.
func LoadDir(dir string, o LoadOptions) (*Plugin, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	file := filepath.Join(abs, ManifestFile)
	f, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: no %s in the plugin directory", abs, ManifestFile)
		}
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", file)
	}
	if err := checkNotWorldWritable(file, st); err != nil {
		return nil, err
	}
	m, err := ParseManifest(file, f)
	if err != nil {
		return nil, err
	}
	if m.MinGTAVersion != "" && o.GTAVersion != "" {
		need, _ := ParseVersion(m.MinGTAVersion)
		have, err := ParseVersion(o.GTAVersion)
		if err == nil && !have.AtLeast(need) {
			return nil, &ValidationError{File: file, Problems: []string{fmt.Sprintf("needs GTA %s or newer; this is %s", m.MinGTAVersion, o.GTAVersion)}}
		}
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	p := &Plugin{Manifest: *m, Dir: abs, Source: o.Source}
	var problems []string
	for _, tm := range m.Tools {
		t, err := newTool(p, tm, o)
		if err != nil {
			problems = append(problems, fmt.Sprintf("tool %q: %v", tm.Name, err))
			continue
		}
		p.Tools = append(p.Tools, t)
	}
	for _, cm := range m.Commands {
		c, err := newCommand(p, cm, o)
		if err != nil {
			problems = append(problems, fmt.Sprintf("command %q: %v", cm.Name, err))
			continue
		}
		p.Commands = append(p.Commands, c)
	}
	if len(problems) > 0 {
		return nil, &ValidationError{File: file, Problems: problems}
	}
	return p, nil
}

// procEnv builds the environment for one process of the plugin.
func (p *Plugin) procEnv(o LoadOptions, gta map[string]string) []string {
	exp := expander(p.Dir, o.Workspace)
	static := make(map[string]string, len(p.Manifest.Env))
	for k, v := range p.Manifest.Env {
		static[k] = exp.Replace(v)
	}
	base := map[string]string{
		"GTA_PLUGIN_NAME":    p.Manifest.Name,
		"GTA_PLUGIN_DIR":     p.Dir,
		"GTA_PLUGIN_VERSION": p.Manifest.Version,
		"GTA_WORKSPACE":      o.Workspace,
		"GTA_VERSION":        o.GTAVersion,
	}
	for k, v := range gta {
		base[k] = v
	}
	return buildEnv(o.Environ(), p.Manifest.PassEnv, static, base)
}

func seconds(n, def int) time.Duration {
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * time.Second
}
