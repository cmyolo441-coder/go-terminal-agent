package supplychain

import (
	"sort"
	"strings"
)

// DamerauLevenshtein returns the unrestricted Damerau-Levenshtein distance
// (insertions, deletions, substitutions and transpositions of adjacent
// characters, where substrings may be edited more than once) between a
// and b, computed over runes (Lowrance-Wagner algorithm).
func DamerauLevenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	maxDist := la + lb
	da := map[rune]int{}
	d := make([][]int, la+2)
	for i := range d {
		d[i] = make([]int, lb+2)
	}
	d[0][0] = maxDist
	for i := 0; i <= la; i++ {
		d[i+1][0] = maxDist
		d[i+1][1] = i
	}
	for j := 0; j <= lb; j++ {
		d[0][j+1] = maxDist
		d[1][j+1] = j
	}
	for i := 1; i <= la; i++ {
		db := 0
		for j := 1; j <= lb; j++ {
			i1 := da[rb[j-1]]
			j1 := db
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
				db = j
			}
			d[i+1][j+1] = min(
				d[i][j]+cost, // substitution
				d[i+1][j]+1,  // insertion
				d[i][j+1]+1,  // deletion
				d[i1][j1]+(i-i1-1)+1+(j-j1-1), // transposition
			)
		}
		da[ra[i-1]] = i
	}
	return d[la+1][lb+1]
}

// confusables maps visually confusable characters (including common
// Cyrillic and Greek homoglyphs) to a canonical skeleton character.
var confusables = map[rune]string{
	'0': "o", '1': "l", '3': "e", '5': "s", '|': "l", 'i': "l",
	'а': "a", 'е': "e", 'о': "o", 'р': "p", 'с': "c", 'у': "y", 'х': "x", 'і': "l", 'ј': "j", 'ѕ': "s",
	'ԁ': "d", 'ɡ': "g", 'ο': "o", 'α': "a", 'ν': "v", 'ρ': "p", 'τ': "t", 'ι': "l", 'κ': "k",
	'-': "", '_': "", '.': "",
}

// skeleton reduces a name to a confusable-insensitive form: lower case,
// separators removed, homoglyphs mapped, and "rn"->"m", "vv"->"w".
func skeleton(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if s, ok := confusables[r]; ok {
			b.WriteString(s)
		} else {
			b.WriteRune(r)
		}
	}
	s := b.String()
	s = strings.ReplaceAll(s, "rn", "m")
	s = strings.ReplaceAll(s, "vv", "w")
	s = strings.ReplaceAll(s, "cl", "d")
	return s
}

// TyposquatMatch describes a suspicious similarity to a popular package.
type TyposquatMatch struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	Distance int    `json:"distance"`
	// Reason is "edit-distance", "confusable" or "scope-confusion".
	Reason string `json:"reason"`
}

// TyposquatChecker compares names against a list of popular packages.
type TyposquatChecker struct {
	popular map[Ecosystem][]string
	set     map[Ecosystem]map[string]bool
	// MaxDistance is the edit distance flagged for names of at least
	// LongNameLen characters; shorter names use distance 1 (default 2/8).
	MaxDistance int
	LongNameLen int
	// MinNameLen skips very short names, which collide too often (default 4).
	MinNameLen int
}

// NewTyposquatChecker builds a checker. A nil list uses the embedded
// sample of popular npm and PyPI packages.
func NewTyposquatChecker(popular map[Ecosystem][]string) *TyposquatChecker {
	if popular == nil {
		popular = map[Ecosystem][]string{EcosystemNPM: topNPM, EcosystemPyPI: topPyPI}
	}
	t := &TyposquatChecker{popular: map[Ecosystem][]string{}, set: map[Ecosystem]map[string]bool{},
		MaxDistance: 2, LongNameLen: 8, MinNameLen: 4}
	for eco, names := range popular {
		t.set[eco] = map[string]bool{}
		for _, n := range names {
			n = typoNorm(eco, n)
			if !t.set[eco][n] {
				t.set[eco][n] = true
				t.popular[eco] = append(t.popular[eco], n)
			}
		}
		sort.Strings(t.popular[eco])
	}
	return t
}

func typoNorm(eco Ecosystem, n string) string {
	if eco == EcosystemPyPI {
		return NormalizePyPIName(n)
	}
	return strings.ToLower(n)
}

// Check returns the closest popular package name resembles, or nil.
func (t *TyposquatChecker) Check(eco Ecosystem, name string) *TyposquatMatch {
	pop := t.popular[eco]
	if len(pop) == 0 {
		return nil
	}
	n := typoNorm(eco, name)
	if t.set[eco][n] {
		return nil
	}
	// scope confusion: "@evil/lodash" or "lodash-js" style wrappers are
	// left to the distance checks; "@types/x" is legitimate.
	bare := n
	if eco == EcosystemNPM && strings.HasPrefix(n, "@") {
		if i := strings.IndexByte(n, '/'); i > 0 {
			bare = n[i+1:]
			scope := n[:i]
			if t.set[eco][bare] && scope != "@types" && !t.set[eco][n] {
				return &TyposquatMatch{Name: name, Target: bare, Distance: 0, Reason: "scope-confusion"}
			}
		}
	}
	if len([]rune(bare)) < t.MinNameLen {
		return nil
	}
	sk := skeleton(bare)
	var best *TyposquatMatch
	for _, p := range pop {
		if p == bare {
			return nil // scoped copy of a popular name already handled
		}
		if skeleton(p) == sk {
			return &TyposquatMatch{Name: name, Target: p, Distance: DamerauLevenshtein(bare, p), Reason: "confusable"}
		}
		limit := 1
		if len([]rune(p)) >= t.LongNameLen {
			limit = t.MaxDistance
		}
		if abs(len(p)-len(bare)) > limit {
			continue
		}
		d := DamerauLevenshtein(bare, p)
		if d > 0 && d <= limit && (best == nil || d < best.Distance) {
			best = &TyposquatMatch{Name: name, Target: p, Distance: d, Reason: "edit-distance"}
		}
	}
	return best
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Small embedded samples of very popular package names. Callers with a
// fuller list should pass it to NewTyposquatChecker.
var topNPM = []string{
	"lodash", "react", "react-dom", "express", "axios", "chalk", "commander", "debug", "moment",
	"request", "webpack", "typescript", "underscore", "async", "bluebird", "uuid", "classnames",
	"prop-types", "vue", "jquery", "yargs", "dotenv", "body-parser", "fs-extra", "glob", "minimist",
	"semver", "rimraf", "mkdirp", "colors", "inquirer", "eslint", "prettier", "jest", "mocha",
	"babel-core", "@babel/core", "rxjs", "redux", "next", "socket.io", "cross-env", "nodemon",
	"core-js", "tslib", "node-fetch", "cheerio", "mongoose", "jsonwebtoken", "bcrypt", "cors",
	"electron", "puppeteer", "angular", "zod", "esbuild", "vite", "styled-components", "ws",
	"qs", "yaml", "js-yaml", "ramda", "immer", "dayjs", "date-fns", "luxon", "nanoid", "sharp",
	"handlebars", "ejs", "pug", "passport", "helmet", "morgan", "winston", "pino", "graphql",
	"apollo-server", "sequelize", "knex", "pg", "mysql", "mysql2", "redis", "ioredis",
}

var topPyPI = []string{
	"requests", "urllib3", "numpy", "pandas", "boto3", "botocore", "setuptools", "six", "certifi",
	"idna", "charset-normalizer", "python-dateutil", "pyyaml", "typing-extensions", "packaging",
	"cryptography", "pip", "wheel", "attrs", "jinja2", "markupsafe", "click", "flask", "django",
	"pytest", "scipy", "matplotlib", "pillow", "sqlalchemy", "pydantic", "fastapi", "uvicorn",
	"aiohttp", "beautifulsoup4", "lxml", "protobuf", "grpcio", "tensorflow", "torch", "scikit-learn",
	"psycopg2", "psycopg2-binary", "pymysql", "redis", "celery", "httpx", "rich", "tqdm", "colorama",
	"jsonschema", "pyjwt", "paramiko", "docutils", "openpyxl", "selenium", "gunicorn", "werkzeug",
	"pytz", "tzdata", "filelock", "virtualenv", "platformdirs", "toml", "tomli", "pyparsing",
	"simplejson", "decorator", "websocket-client", "openai", "anthropic", "transformers", "keras",
}
