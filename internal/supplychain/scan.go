package supplychain

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ScanOptions configures ScanWorkspace.
type ScanOptions struct {
	// SkipDirs are directory base names never descended into, in addition
	// to the defaults (.git, node_modules, vendor, target, .venv, ...).
	SkipDirs []string
	// MaxFileSize bounds the size of a manifest read (default 64 MiB).
	MaxFileSize int64
	// MaxDepth bounds directory depth below root (default 32).
	MaxDepth int
}

// ScanResult is the outcome of ScanWorkspace.
type ScanResult struct {
	Graph *Graph
	// Manifests lists the slash-separated paths (relative to root) parsed.
	Manifests []string
	// InstallScripts maps package.json paths to the install-time lifecycle
	// scripts the workspace itself declares.
	InstallScripts map[string]map[string]string
	// Errors collects per-file parse errors; scanning continues past them.
	Errors []error
}

var defaultSkipDirs = []string{".git", ".hg", ".svn", "node_modules", "vendor", "target", ".venv", "venv",
	"__pycache__", ".tox", ".mypy_cache", "bower_components", ".next", "dist", ".gradle"}

// ScanWorkspace walks root, parses every recognised manifest and lockfile
// and merges the results into one graph. Files are read through an
// os.Root so symlinks cannot escape the workspace.
func ScanWorkspace(root string, opts ScanOptions) (*ScanResult, error) {
	if opts.MaxFileSize <= 0 {
		opts.MaxFileSize = 64 << 20
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 32
	}
	skip := map[string]bool{}
	for _, d := range append(append([]string(nil), defaultSkipDirs...), opts.SkipDirs...) {
		skip[d] = true
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("supplychain: open workspace: %w", err)
	}
	defer r.Close()
	res := &ScanResult{Graph: NewGraph(), InstallScripts: map[string]map[string]string{}}
	dirs := map[string]map[string]bool{} // rel dir -> file names
	err = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			res.Errors = append(res.Errors, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != "." && (skip[d.Name()] || strings.Count(p, "/")+1 > opts.MaxDepth) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !isManifestName(p) {
			return nil
		}
		dir := path.Dir(p)
		if dirs[dir] == nil {
			dirs[dir] = map[string]bool{}
		}
		dirs[dir][path.Base(p)] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	dirNames := make([]string, 0, len(dirs))
	for d := range dirs {
		dirNames = append(dirNames, d)
	}
	sort.Strings(dirNames)
	s := &scanner{root: r, opts: opts, res: res, visitedReq: map[string]bool{}}
	for _, d := range dirNames {
		s.scanDir(d, dirs[d])
	}
	sort.Strings(res.Manifests)
	return res, nil
}

func isManifestName(p string) bool {
	base := path.Base(p)
	switch base {
	case "go.mod", "package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock",
		"pnpm-lock.yaml", "poetry.lock", "Pipfile.lock", "Cargo.lock", "Gemfile.lock", "composer.lock":
		return true
	}
	if strings.HasSuffix(base, ".txt") && (strings.HasPrefix(base, "requirements") || path.Base(path.Dir(p)) == "requirements") {
		return true
	}
	return false
}

type scanner struct {
	root       *os.Root
	opts       ScanOptions
	res        *ScanResult
	visitedReq map[string]bool
}

func (s *scanner) read(p string) ([]byte, error) {
	f, err := s.root.Open(filepath.FromSlash(p))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, s.opts.MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.opts.MaxFileSize {
		return nil, fmt.Errorf("%s: exceeds %d bytes", p, s.opts.MaxFileSize)
	}
	return data, nil
}

// readOptional returns nil data (and no error) when the file is absent.
func (s *scanner) readOptional(p string) ([]byte, error) {
	data, err := s.read(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

func (s *scanner) merge(file string, g *Graph, err error) {
	if err != nil {
		s.res.Errors = append(s.res.Errors, err)
		return
	}
	s.res.Manifests = append(s.res.Manifests, file)
	s.res.Graph.Merge(g)
}

func (s *scanner) scanDir(dir string, files map[string]bool) {
	join := func(n string) string { return path.Join(dir, n) }
	if files["go.mod"] {
		g, err := s.goModule(join("go.mod"), join("go.sum"))
		s.merge(join("go.mod"), g, err)
	}
	var manifest *PackageJSON
	if files["package.json"] {
		data, err := s.read(join("package.json"))
		if err == nil {
			manifest, err = ParsePackageJSON(join("package.json"), data)
		}
		if err != nil {
			s.res.Errors = append(s.res.Errors, err)
		} else if scripts := manifest.InstallScripts(); len(scripts) > 0 {
			s.res.InstallScripts[join("package.json")] = scripts
		}
	}
	locked := false
	for _, lock := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
		if files[lock] {
			locked = true
			data, err := s.read(join(lock))
			var g *Graph
			if err == nil {
				g, err = ParsePackageLock(join(lock), data, manifest)
			}
			s.merge(join(lock), g, err)
		}
	}
	if files["yarn.lock"] {
		locked = true
		data, err := s.read(join("yarn.lock"))
		var g *Graph
		if err == nil {
			g, err = ParseYarnLock(join("yarn.lock"), data, manifest)
		}
		s.merge(join("yarn.lock"), g, err)
	}
	if files["pnpm-lock.yaml"] {
		locked = true
		data, err := s.read(join("pnpm-lock.yaml"))
		var g *Graph
		if err == nil {
			g, err = ParsePNPMLock(join("pnpm-lock.yaml"), data)
		}
		s.merge(join("pnpm-lock.yaml"), g, err)
	}
	if manifest != nil && !locked && !s.insideLockedWorkspace(dir) {
		s.merge(join("package.json"), packageJSONGraph(join("package.json"), manifest), nil)
	}
	if files["poetry.lock"] {
		data, err := s.read(join("poetry.lock"))
		var g *Graph
		if err == nil {
			var py []byte
			if py, err = s.readOptional(join("pyproject.toml")); err == nil {
				g, err = ParsePoetryLock(join("poetry.lock"), data, py)
			}
		}
		s.merge(join("poetry.lock"), g, err)
	}
	if files["Pipfile.lock"] {
		data, err := s.read(join("Pipfile.lock"))
		var g *Graph
		if err == nil {
			var pf []byte
			if pf, err = s.readOptional(join("Pipfile")); err == nil {
				g, err = ParsePipfileLock(join("Pipfile.lock"), data, pf)
			}
		}
		s.merge(join("Pipfile.lock"), g, err)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.HasSuffix(n, ".txt") {
			s.requirements(join(n), 0)
		}
	}
	if files["Cargo.lock"] {
		data, err := s.read(join("Cargo.lock"))
		var g *Graph
		if err == nil {
			g, err = ParseCargoLock(join("Cargo.lock"), data)
		}
		s.merge(join("Cargo.lock"), g, err)
	}
	if files["Gemfile.lock"] {
		data, err := s.read(join("Gemfile.lock"))
		var g *Graph
		if err == nil {
			g, err = ParseGemfileLock(join("Gemfile.lock"), data)
		}
		s.merge(join("Gemfile.lock"), g, err)
	}
	if files["composer.lock"] {
		data, err := s.read(join("composer.lock"))
		var g *Graph
		if err == nil {
			var cj []byte
			if cj, err = s.readOptional(join("composer.json")); err == nil {
				g, err = ParseComposerLock(join("composer.lock"), data, cj)
			}
		}
		s.merge(join("composer.lock"), g, err)
	}
}

// insideLockedWorkspace reports whether an ancestor directory has an npm
// lockfile (workspace members are covered by the root lockfile).
func (s *scanner) insideLockedWorkspace(dir string) bool {
	for d := dir; d != "." && d != "/" && d != ""; {
		d = path.Dir(d)
		for _, lock := range []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "npm-shrinkwrap.json"} {
			if f, err := s.root.Open(filepath.FromSlash(path.Join(d, lock))); err == nil {
				f.Close()
				return true
			}
		}
	}
	return false
}

func (s *scanner) goModule(modPath, sumPath string) (*Graph, error) {
	data, err := s.read(modPath)
	if err != nil {
		return nil, err
	}
	mod, err := ParseGoMod(modPath, data)
	if err != nil {
		return nil, err
	}
	sumData, err := s.readOptional(sumPath)
	if err != nil {
		return nil, err
	}
	var sum []GoSumLine
	if sumData != nil {
		if sum, err = ParseGoSum(sumPath, sumData); err != nil {
			return nil, err
		}
	}
	return GoGraph(modPath, mod, sum), nil
}

func (s *scanner) requirements(p string, depth int) {
	p = path.Clean(p)
	if s.visitedReq[p] || depth > 16 || strings.HasPrefix(p, "../") || p == ".." {
		return
	}
	s.visitedReq[p] = true
	data, err := s.read(p)
	if err != nil {
		s.res.Errors = append(s.res.Errors, err)
		return
	}
	rf, err := ParseRequirements(p, data)
	if err != nil {
		s.res.Errors = append(s.res.Errors, err)
		return
	}
	s.merge(p, RequirementsGraph(p, rf), nil)
	for _, inc := range rf.Includes {
		if strings.Contains(inc, "://") {
			continue
		}
		s.requirements(path.Join(path.Dir(p), filepath.ToSlash(inc)), depth+1)
	}
}

// packageJSONGraph records unlocked npm dependencies from package.json.
func packageJSONGraph(file string, m *PackageJSON) *Graph {
	g := NewGraph()
	add := func(deps map[string]string, dev, optional bool) {
		for name, rng := range deps {
			g.Add(&Package{Ecosystem: EcosystemNPM, Name: name, Direct: true, Dev: dev, Optional: optional,
				Unpinned: true, Constraint: rng, Sources: []string{file}})
		}
	}
	add(m.Dependencies, false, false)
	add(m.OptionalDependencies, false, true)
	add(m.DevDependencies, true, false)
	return g
}
