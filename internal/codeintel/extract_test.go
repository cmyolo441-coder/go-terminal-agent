package codeintel

import (
	"fmt"
	"strings"
	"testing"
)

// symKey renders a symbol compactly for table assertions.
func symKey(s Symbol) string {
	return fmt.Sprintf("%s %s %d-%d", s.Kind, s.QualifiedName(), s.Line, s.EndLine)
}

func symKeys(syms []Symbol) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = symKey(s)
	}
	return out
}

func findSym(t *testing.T, syms []Symbol, qname string) Symbol {
	t.Helper()
	for _, s := range syms {
		if s.QualifiedName() == qname {
			return s
		}
	}
	t.Fatalf("symbol %q not found in:\n%s", qname, strings.Join(symKeys(syms), "\n"))
	return Symbol{}
}

func assertKeys(t *testing.T, syms []Symbol, want ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, k := range symKeys(syms) {
		have[k] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing %q; have:\n%s", w, strings.Join(symKeys(syms), "\n"))
		}
	}
}

func refuseKeys(t *testing.T, syms []Symbol, names ...string) {
	t.Helper()
	for _, s := range syms {
		for _, n := range names {
			if s.Name == n {
				t.Errorf("unexpected symbol %s", symKey(s))
			}
		}
	}
}

func TestExtractGo(t *testing.T) {
	src := `// Package demo is a demo.
package demo

import "fmt"

// MaxSize bounds things.
const MaxSize = 10

const (
	// A is first.
	A = iota
	B // trailing
)

var registry = map[string]int{
	"a": 1,
}

// Server serves.
// It has two lines of doc.
type Server[K comparable] struct {
	Name string // the name
	port int
}

// Handler handles requests.
type Handler interface {
	// Serve one.
	Serve(req string) error
	fmt.Stringer
}

type ID = string

// Start runs the server.
func (s *Server[K]) Start(addr string) error {
	return nil
}

func helper(x int) (int, error) {
	return x, nil
}
`
	syms := ExtractSymbols(LangGo, src)
	assertKeys(t, syms,
		"const MaxSize 7-7",
		"const A 11-11",
		"const B 12-12",
		"var registry 15-17",
		"struct Server 21-24",
		"field Server.Name 22-22",
		"field Server.port 23-23",
		"interface Handler 27-31",
		"method Handler.Serve 29-29",
		"type ID 33-33",
		"method Server.Start 36-38",
		"function helper 40-42",
	)
	srv := findSym(t, syms, "Server")
	if srv.DocLine != 19 || srv.Doc != "Server serves.\nIt has two lines of doc." {
		t.Errorf("Server doc = %d %q", srv.DocLine, srv.Doc)
	}
	if srv.Signature != "type Server[K comparable] struct" {
		t.Errorf("Server signature = %q", srv.Signature)
	}
	start := findSym(t, syms, "Server.Start")
	if start.Signature != "func (s *Server[K]) Start(addr string) error" || !start.Exported || start.Doc != "Start runs the server." {
		t.Errorf("Start = %+v", start)
	}
	if b := findSym(t, syms, "B"); b.Doc != "trailing" {
		t.Errorf("B doc = %q", b.Doc)
	}
	if h := findSym(t, syms, "helper"); h.Exported {
		t.Error("helper should not be exported")
	}
}

func TestExtractGoBrokenFileFallsBack(t *testing.T) {
	// No package clause: go/parser yields no AST at all.
	src := "func Broken( {\n}\n\ntype Thing struct {\n\tA int\n}\n\nfunc (t *Thing) Do() {}\n"
	syms := ExtractSymbols(LangGo, src)
	assertKeys(t, syms, "struct Thing 4-6", "method Thing.Do 8-8")
}

func TestExtractGoPartialAST(t *testing.T) {
	src := "package p\n\nfunc Good() {}\n\nfunc Bad( {\n\nfunc Later() {}\n"
	syms := ExtractSymbols(LangGo, src)
	findSym(t, syms, "Good")
	findSym(t, syms, "Later")
}

func TestExtractPython(t *testing.T) {
	src := `"""Module doc with def fake(): inside."""
import os

MAX_RETRIES: int = 3


@dataclass
class Config(Base):
    """Holds settings.

    More detail.
    """

    name: str = "x"

    def load(self, path):
        # comment with def nope():
        def inner():
            return 1
        return inner()

    @property
    def size(
        self,
        extra=None,
    ) -> int:
        '''Size in bytes.'''
        return 0


async def fetch(url: str) -> bytes:
    s = """
def not_a_function():
    pass
"""
    return b""


def _private():
    pass
`
	syms := ExtractSymbols(LangPython, src)
	assertKeys(t, syms,
		"const MAX_RETRIES 4-4",
		"class Config 8-28",
		"method Config.load 16-20",
		"method Config.size 23-28",
		"function fetch 31-36",
		"function _private 39-40",
	)
	refuseKeys(t, syms, "inner", "not_a_function", "nope", "fake")
	cfg := findSym(t, syms, "Config")
	if cfg.Doc != "Holds settings.\n\nMore detail." || cfg.DocLine != 7 {
		t.Errorf("Config doc = %d %q", cfg.DocLine, cfg.Doc)
	}
	size := findSym(t, syms, "Config.size")
	if size.Doc != "Size in bytes." {
		t.Errorf("size doc = %q", size.Doc)
	}
	if !strings.HasPrefix(size.Signature, "def size( self, extra=None, ) -> int") {
		t.Errorf("size signature = %q", size.Signature)
	}
	if findSym(t, syms, "_private").Exported {
		t.Error("_private exported")
	}
}

func TestExtractTypeScript(t *testing.T) {
	src := "import fs from 'fs';\n" + // 1
		"const path = require('path');\n" + // 2
		"\n" + // 3
		"/** Options for the widget. */\n" + // 4
		"export interface WidgetOptions {\n" + // 5
		"  name: string;\n" + // 6
		"  render(x: number): void;\n" + // 7
		"}\n" + // 8
		"\n" + // 9
		"export type Mode = 'a' | 'b';\n" + // 10
		"\n" + // 11
		"export enum Color { Red, Green }\n" + // 12
		"\n" + // 13
		"// Widget draws things.\n" + // 14
		"@Component({ selector: 'w' })\n" + // 15
		"export class Widget extends Base {\n" + // 16
		"  private count: number;\n" + // 17
		"  static create(opts: WidgetOptions): Widget {\n" + // 18
		"    if (opts) { return new Widget(); }\n" + // 19
		"    const t = `template ${opts.name} { not a brace`;\n" + // 20
		"    return null;\n" + // 21
		"  }\n" + // 22
		"  async load(): Promise<void> {\n" + // 23
		"    const re = /[{}]+/g;\n" + // 24
		"  }\n" + // 25
		"  onClick = (e: Event) => {\n" + // 26
		"    this.load();\n" + // 27
		"  };\n" + // 28
		"}\n" + // 29
		"\n" + // 30
		"export const handler = async (req, res) => {\n" + // 31
		"  function nested() {}\n" + // 32
		"};\n" + // 33
		"\n" + // 34
		"export function* gen() {\n" + // 35
		"  yield 1;\n" + // 36
		"}\n" + // 37
		"export const LIMIT = 5;\n" // 38
	syms := ExtractSymbols(LangTypeScript, src)
	assertKeys(t, syms,
		"interface WidgetOptions 5-8",
		"field WidgetOptions.name 6-6",
		"method WidgetOptions.render 7-7",
		"type Mode 10-10",
		"enum Color 12-12",
		"class Widget 16-29",
		"field Widget.count 17-17",
		"method Widget.create 18-22",
		"method Widget.load 23-25",
		"method Widget.onClick 26-28",
		"function handler 31-33",
		"function gen 35-37",
		"const LIMIT 38-38",
	)
	refuseKeys(t, syms, "path", "nested", "fs", "t", "re")
	w := findSym(t, syms, "Widget")
	if w.Doc != "Widget draws things." || w.DocLine != 14 {
		t.Errorf("Widget doc = %d %q", w.DocLine, w.Doc)
	}
	if opts := findSym(t, syms, "WidgetOptions"); opts.Doc != "Options for the widget." {
		t.Errorf("WidgetOptions doc = %q", opts.Doc)
	}
	if c := findSym(t, syms, "Widget.create"); c.Signature != "static create(opts: WidgetOptions): Widget" {
		t.Errorf("create signature = %q", c.Signature)
	}
}

func TestExtractJavaScriptCommonJS(t *testing.T) {
	src := `'use strict';
const express = require('express');

function main() {
  const app = express();
  app.get('/', (req, res) => {
    res.send('{');
  });
}

module.exports = { main };
`
	syms := ExtractSymbols(LangJavaScript, src)
	assertKeys(t, syms, "function main 4-9")
	refuseKeys(t, syms, "express", "app")
}

func TestExtractRust(t *testing.T) {
	src := `use std::fmt;

/// A point in space.
#[derive(Debug, Clone)]
pub struct Point<'a> {
    pub x: f64,
    label: &'a str,
}

pub trait Shape {
    fn area(&self) -> f64;
    fn name(&self) -> String {
        "shape".to_string()
    }
}

impl<'a> fmt::Display for Point<'a> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let c = '{';
        write!(f, "({}, {})", self.x, c)
    }
}

impl<'a> Point<'a> {
    pub const ORIGIN: f64 = 0.0;

    /// Creates a point.
    pub fn new(x: f64) -> Self
    where
        Self: Sized,
    {
        fn helper() {}
        Point { x, label: "" }
    }
}

pub enum Kind { A, B }

pub mod inner {
    pub fn util() {}
}

macro_rules! square {
    ($x:expr) => { $x * $x };
}

static mut COUNTER: u32 = 0;
pub type Res<T> = Result<T, String>;
const RAW: &str = r#"fn fake() { "#;
`
	syms := ExtractSymbols(LangRust, src)
	assertKeys(t, syms,
		"struct Point 5-8",
		"trait Shape 10-15",
		"method Shape.area 11-11",
		"method Shape.name 12-14",
		"method Point.fmt 18-21",
		"method Point.new 28-34",
		"enum Kind 37-37",
		"module inner 39-41",
		"function inner.util 40-40",
		"macro square 43-45",
		"var COUNTER 47-47",
		"type Res 48-48",
		"const RAW 49-49",
	)
	refuseKeys(t, syms, "helper", "fake")
	p := findSym(t, syms, "Point")
	if p.Doc != "A point in space." || p.DocLine != 3 || !p.Exported {
		t.Errorf("Point = %+v", p)
	}
	if n := findSym(t, syms, "Point.new"); n.Doc != "Creates a point." || n.Signature != "pub fn new(x: f64) -> Self where Self: Sized," {
		t.Errorf("new = %q / %q", n.Doc, n.Signature)
	}
}

func TestExtractJava(t *testing.T) {
	src := `package com.example;

/**
 * Manages accounts.
 */
@Service
public class AccountService implements Service {
    public static final int MAX = 10;
    private final Repo repo = new Repo();

    public AccountService(Repo repo) {
        this.repo = repo;
    }

    @Override
    public <T> List<T> findAll(String query) throws IOException {
        if (query == null) {
            return foo(query);
        }
        return null;
    }

    abstract void hook();

    enum State {
        ACTIVE("a"),
        CLOSED("c");
        State(String s) {}
    }
}

interface Repo {
    Account load(long id);
}
`
	syms := ExtractSymbols(LangJava, src)
	assertKeys(t, syms,
		"class AccountService 7-30",
		"const AccountService.MAX 8-8",
		"method AccountService.AccountService 11-13",
		"method AccountService.findAll 16-21",
		"method AccountService.hook 23-23",
		"enum AccountService.State 25-29",
		"method State.State 28-28",
		"interface Repo 32-34",
		"method Repo.load 33-33",
	)
	refuseKeys(t, syms, "foo", "ACTIVE", "CLOSED", "repo")
	if a := findSym(t, syms, "AccountService"); a.Doc != "Manages accounts." || a.DocLine != 3 || !a.Exported {
		t.Errorf("AccountService = %+v", a)
	}
}

func TestExtractC(t *testing.T) {
	src := `#include <stdio.h>
#define MAX_LEN 64

typedef int (*callback_t)(int);
typedef unsigned long size_type;

/* A node. */
struct node {
    int value;
    struct node *next;
};

typedef struct {
    int x;
} point_t;

static int helper(int a);

/* Adds two numbers. */
int add(int a, int b)
{
    if (a > b) {
        return helper(a);
    }
    return a + b;
}

static int helper(int a) {
    return a * 2;
}

extern "C" {
void exported_fn(void) {}
}
`
	syms := ExtractSymbols(LangC, src)
	assertKeys(t, syms,
		"macro MAX_LEN 2-2",
		"type callback_t 4-4",
		"type size_type 5-5",
		"struct node 8-11",
		"type point_t 15-15",
		"function add 20-26",
		"function helper 28-30",
		"function exported_fn 33-33",
	)
	for _, s := range syms {
		if s.Name == "helper" && s.Line == 17 {
			t.Error("prototype kept although a definition exists")
		}
	}
	if a := findSym(t, syms, "add"); a.Doc != "Adds two numbers." || a.Signature != "int add(int a, int b)" {
		t.Errorf("add = %q %q", a.Doc, a.Signature)
	}
	if findSym(t, syms, "helper").Exported {
		t.Error("static helper should not be exported")
	}
}

func TestExtractCPP(t *testing.T) {
	src := `namespace geo {

class Shape {
public:
    Shape(int sides);
    virtual ~Shape();
    virtual double area() const = 0;
    int sides() const { return sides_; }
private:
    int sides_;
};

Shape::Shape(int sides) : sides_(sides) {}

double Circle::area() const {
    return 3.14 * r_ * r_;
}

template <typename T>
T max_of(T a, T b) {
    return a > b ? a : b;
}

std::vector<int> Shape::corners() const
{
    return {};
}

}  // namespace geo
`
	syms := ExtractSymbols(LangCPP, src)
	assertKeys(t, syms,
		"module geo 1-29",
		"class geo.Shape 3-11",
		"method Shape.Shape 13-13",
		"method Shape.~Shape 6-6",
		"method Shape.area 7-7",
		"method Shape.sides 8-8",
		"method Circle.area 15-17",
		"function geo.max_of 20-22",
		"method Shape.corners 24-27",
	)
	if m := findSym(t, syms, "geo.max_of"); m.DocLine != 19 {
		t.Errorf("template line should be part of the doc block: %+v", m)
	}
}

func TestExtractRuby(t *testing.T) {
	src := `# Billing helpers.
module Billing
  RATE = 0.2

  # An invoice.
  class Invoice < Base
    attr_reader :total

    def initialize(total)
      @total = total
    end

    def self.build(x) = new(x)

    def paid?
      if total > 0
        true
      end
    end

    class << self
      def registry; @registry ||= {}; end
    end
  end
end
`
	syms := ExtractSymbols(LangRuby, src)
	assertKeys(t, syms,
		"module Billing 2-25",
		"const Billing.RATE 3-3",
		"class Billing.Invoice 6-24",
		"method Invoice.initialize 9-11",
		"method Invoice.build 13-13",
		"method Invoice.paid? 15-19",
		"method Invoice.registry 22-22",
	)
	if inv := findSym(t, syms, "Billing.Invoice"); inv.Doc != "An invoice." {
		t.Errorf("Invoice doc = %q", inv.Doc)
	}
}

func TestExtractMarkdown(t *testing.T) {
	src := "# Title\n\nintro\n\n## Install\n\nsteps\n\n```sh\n# not a heading\n```\n\n## Usage\n\ntext\n"
	syms := ExtractSymbols(LangMarkdown, src)
	assertKeys(t, syms, "section Title 1-15", "section Install 5-11", "section Usage 13-15")
	refuseKeys(t, syms, "not a heading")
}

func TestDetectLanguage(t *testing.T) {
	cases := map[string]string{
		"a/b.go": LangGo, "x.PY": LangPython, "c.tsx": LangTypeScript, "d.mjs": LangJavaScript,
		"lib.rs": LangRust, "A.java": LangJava, "m.h": LangC, "m.hpp": LangCPP, "Rakefile": LangRuby,
		"README.md": LangMarkdown, "notes.txt": LangText, "Makefile": LangText,
	}
	for in, want := range cases {
		if got := DetectLanguage(in); got != want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLexerMasksStringsAndComments(t *testing.T) {
	src := "let a = \"{\"; // }\nlet b = '}'; /* {\n{ */ let c = `x ${ {a:1}.a } {`;\n"
	lx := lexSource(src, lexSpecs[LangJavaScript])
	opens, closes := 0, 0
	for _, e := range lx.events {
		switch e.kind {
		case '{':
			opens++
		case '}':
			closes++
		}
	}
	// Only the object literal inside ${} is code.
	if opens != 1 || closes != 1 {
		t.Fatalf("opens=%d closes=%d masked=%q", opens, closes, lx.masked)
	}
	if strings.Contains(lx.masked[0], "//") || strings.Contains(lx.masked[0], "\"{\"") {
		t.Errorf("line 0 not masked: %q", lx.masked[0])
	}
	if len(lx.masked) != len(lx.raw) {
		t.Fatal("masking changed the line count")
	}
	for i := range lx.raw {
		if len(lx.raw[i]) != len(lx.masked[i]) {
			t.Errorf("line %d length changed", i)
		}
	}
}

func TestExtractNeverPanicsOnGarbage(t *testing.T) {
	inputs := []string{
		"", "{", "}", "\"", "'", "`", "/*", "r#\"", "def", "class", "fn (", "}}}{{{", "'\\", "\"\\",
		"`${", "`${`${", "impl<", "#define", "template <", "=begin\n", "def x(\n", "struct {",
	}
	for lang := range langPatterns {
		for _, in := range inputs {
			for _, s := range []string{in, in + "\n" + in, strings.Repeat(in, 50)} {
				_ = ExtractSymbols(lang, s)
			}
		}
	}
	for _, in := range inputs {
		_ = ExtractSymbols(LangMarkdown, in)
	}
}
