package autopilot

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// want describes an expected failure; zero fields are not checked.
type want struct {
	kind    FailureKind
	suite   string
	test    string
	file    string
	line    int
	message string // substring
}

func checkFailures(t *testing.T, got []Failure, wants []want) {
	t.Helper()
	if len(got) != len(wants) {
		for _, f := range got {
			t.Logf("got: %+v", f)
		}
		t.Fatalf("got %d failures, want %d", len(got), len(wants))
	}
	for i, w := range wants {
		f := got[i]
		if w.kind != "" && f.Kind != w.kind {
			t.Errorf("failure %d kind %q, want %q", i, f.Kind, w.kind)
		}
		if w.suite != "" && f.Suite != w.suite {
			t.Errorf("failure %d suite %q, want %q", i, f.Suite, w.suite)
		}
		if w.test != "" && f.Test != w.test {
			t.Errorf("failure %d test %q, want %q", i, f.Test, w.test)
		}
		if w.file != "" && f.File != w.file {
			t.Errorf("failure %d file %q, want %q", i, f.File, w.file)
		}
		if w.line != 0 && f.Line != w.line {
			t.Errorf("failure %d line %d, want %d", i, f.Line, w.line)
		}
		if w.message != "" && !strings.Contains(f.Message, w.message) {
			t.Errorf("failure %d message %q does not contain %q", i, f.Message, w.message)
		}
	}
}

var goWant = []want{
	{kind: KindBuild, suite: "example.com/demo/bad", file: "bad/b.go", line: 3, message: "undefined: undefinedThing"},
	{kind: KindTest, suite: "example.com/demo", test: "TestAdd", file: "calc_test.go", line: 7, message: "Add(2, 3) = -1, want 5\nsecond line"},
	{kind: KindTest, suite: "example.com/demo", test: "TestTable/neg", file: "calc_test.go", line: 14, message: "neg broken"},
	{kind: KindTest, suite: "example.com/demo/pan", test: "TestPanic", file: "/work/demo/pan/p_test.go", line: 5, message: "panic: assignment to entry in nil map"},
	{kind: KindTest, suite: "example.com/demo/sub", test: "TestSub", file: "s_test.go", line: 5, message: "sub fails"},
}

func TestParseGoPlain(t *testing.T) {
	// Real `go test ./...` output: a build error, a failing test with a
	// multi-line message, a failing subtest (its parent is dropped), a nil
	// map panic located in user code and a failure in another package.
	checkFailures(t, ParseFailures(FrameworkGo, readTestdata(t, "go_plain.txt")), goWant)
}

func TestParseGoJSON(t *testing.T) {
	checkFailures(t, ParseFailures(FrameworkGo, readTestdata(t, "go_json.txt")), goWant)
}

func TestParseGoVerbose(t *testing.T) {
	// With -v the log lines print before "--- FAIL".
	checkFailures(t, ParseFailures(FrameworkGo, readTestdata(t, "go_verbose.txt")), []want{
		{test: "TestAdd", file: "calc_test.go", line: 7, message: "second line"},
		{test: "TestTable/neg", file: "calc_test.go", line: 14, message: "neg broken"},
		{test: "TestSub", suite: "example.com/demo/sub", file: "s_test.go", line: 5},
	})
}

func TestParseGoTimeout(t *testing.T) {
	checkFailures(t, ParseFailures(FrameworkGo, readTestdata(t, "go_timeout.txt")), []want{
		{kind: KindTest, suite: "example.com/to", test: "TestSlow", message: "timed out"},
	})
}

func TestParseGoPackageFailureWithoutTest(t *testing.T) {
	out := "some setup log\nTestMain: database unreachable\nFAIL\texample.com/m/db\t0.010s\nok  \texample.com/m/other\t0.002s\n"
	checkFailures(t, ParseFailures(FrameworkGo, out), []want{
		{kind: KindError, suite: "example.com/m/db", message: "database unreachable"},
	})
}

func TestParsePytest(t *testing.T) {
	checkFailures(t, ParseFailures(FrameworkPytest, readTestdata(t, "pytest_long.txt")), []want{
		{kind: KindTest, test: "test_add", file: "tests/test_calc.py", line: 5, message: "assert -1 == 5"},
		// The innermost frame is the code under test.
		{kind: KindTest, test: "TestDiv.test_zero", file: "calc.py", line: 5, message: "ZeroDivisionError: division by zero"},
		{kind: KindTest, test: "test_param[1-1]", file: "tests/test_calc.py", line: 13, message: "assert 0 == (1 + 1)"},
		{kind: KindTest, test: "test_param[2-2]", file: "tests/test_calc.py", line: 13},
	})
}

func TestParsePytestShortTraceback(t *testing.T) {
	got := ParseFailures(FrameworkPytest, readTestdata(t, "pytest_short.txt"))
	checkFailures(t, got, []want{
		{test: "test_add", file: "tests/test_calc.py", line: 5},
		{test: "TestDiv.test_zero", file: "calc.py", line: 5, message: "ZeroDivisionError"},
		{test: "test_param[1-1]", line: 13},
		{test: "test_param[2-2]", line: 13},
	})
}

func TestParsePytestCollectionError(t *testing.T) {
	checkFailures(t, ParseFailures(FrameworkPytest, readTestdata(t, "pytest_collect.txt")), []want{
		{kind: KindError, file: "tests/test_broken.py", line: 1, message: "No module named 'nonexistent_module_xyz'"},
	})
}

func TestParsePytestSummaryOnly(t *testing.T) {
	out := "tests/test_a.py F\n=========================== short test summary info ============================\nFAILED tests/test_a.py::test_x - KeyError: 'k'\n"
	checkFailures(t, ParseFailures(FrameworkPytest, out), []want{
		{kind: KindTest, test: "test_x", file: "tests/test_a.py", message: "KeyError"},
	})
}

func TestParseJest(t *testing.T) {
	checkFailures(t, ParseFailures(FrameworkJest, readTestdata(t, "jest.txt")), []want{
		{kind: KindError, suite: "src/broken.test.js", file: "src/broken.test.js", line: 1, message: "Cannot find module './does-not-exist'"},
		{kind: KindTest, suite: "src/sum.test.js", test: "sum › adds 1 + 2 to equal 3", file: "src/sum.test.js", line: 5, message: "Expected: 3\nReceived: -1"},
	})
}

func TestParseVitest(t *testing.T) {
	checkFailures(t, ParseFailures(FrameworkVitest, readTestdata(t, "vitest.txt")), []want{
		{kind: KindTest, suite: "v/sum.test.ts", test: "sum > adds", file: "v/sum.test.ts", line: 7, message: "expected -1 to be 3"},
	})
}

func TestParseVitestListingFallback(t *testing.T) {
	out := " ❯ src/a.test.ts (3 tests | 1 failed) 12ms\n   × math > divides 3ms\n     → expected 1 to be 2\n"
	checkFailures(t, ParseFailures(FrameworkVitest, out), []want{
		{suite: "src/a.test.ts", test: "math > divides", file: "src/a.test.ts"},
	})
}

func TestParseCargo(t *testing.T) {
	checkFailures(t, ParseFailures(FrameworkCargo, readTestdata(t, "cargo_fail.txt")), []want{
		{kind: KindTest, test: "tests::it_adds", file: "src/lib.rs", line: 9, message: "left: 0\n right: 4"},
		{kind: KindTest, test: "tests::it_panics", file: "src/lib.rs", line: 15, message: "index out of bounds"},
	})
}

func TestParseCargoOldPanicFormat(t *testing.T) {
	out := "test t::a ... FAILED\n\nfailures:\n\n---- t::a stdout ----\nthread 't::a' panicked at 'boom', src/x.rs:3:5\nnote: run with `RUST_BACKTRACE=1`\n\nfailures:\n    t::a\n"
	checkFailures(t, ParseFailures(FrameworkCargo, out), []want{{test: "t::a", file: "src/x.rs", line: 3, message: "boom"}})
}

func TestParseCargoBuildError(t *testing.T) {
	// "could not compile" summaries carry no location and are not failures.
	checkFailures(t, ParseFailures(FrameworkCargo, readTestdata(t, "cargo_build.txt")), []want{
		{kind: KindBuild, file: "src/lib.rs", line: 1, message: "E0425: cannot find value `c` in this scope"},
	})
}

func TestParseMaven(t *testing.T) {
	out := `[INFO] Running com.ex.CalcTest
[ERROR] Tests run: 2, Failures: 1, Errors: 1, Skipped: 0, Time elapsed: 0.03 s <<< FAILURE! -- in com.ex.CalcTest
[ERROR] com.ex.CalcTest.adds -- Time elapsed: 0.01 s <<< FAILURE!
org.opentest4j.AssertionFailedError: expected: <5> but was: <-1>
[INFO]
[INFO] Results:
[INFO]
[ERROR] Failures:
[ERROR]   CalcTest.adds:5 expected: <5> but was: <-1>
[ERROR] Errors:
[ERROR]   CalcTest.npe:6 NullPointer Cannot invoke "String.length()" because "<local1>" is null
[INFO]
[ERROR] Tests run: 2, Failures: 1, Errors: 1, Skipped: 0
`
	checkFailures(t, ParseFailures(FrameworkMaven, out), []want{
		{kind: KindTest, suite: "CalcTest", test: "adds", file: "CalcTest.java", line: 5, message: "expected: <5> but was: <-1>"},
		{kind: KindTest, suite: "CalcTest", test: "npe", line: 6, message: "NullPointer"},
	})
}

func TestParseMavenCompileError(t *testing.T) {
	out := "[ERROR] COMPILATION ERROR : \n[ERROR] /work/mvn/src/main/java/com/ex/Calc.java:[2,62] cannot find symbol\n"
	checkFailures(t, ParseFailures(FrameworkMaven, out), []want{
		{kind: KindBuild, file: "/work/mvn/src/main/java/com/ex/Calc.java", line: 2, message: "cannot find symbol"},
	})
}

func TestParseGradle(t *testing.T) {
	out := `> Task :test FAILED

CalcTest > adds() FAILED
    org.opentest4j.AssertionFailedError at CalcTest.java:12

CalcTest > other() FAILED
    java.lang.IllegalStateException: bad state
        at com.ex.Calc.run(Calc.kt:3)

2 tests completed, 2 failed
src/main/java/com/ex/Calc.java:4: error: ';' expected
`
	checkFailures(t, ParseFailures(FrameworkGradle, out), []want{
		{kind: KindTest, suite: "CalcTest", test: "adds()", file: "CalcTest.java", line: 12, message: "AssertionFailedError"},
		{kind: KindTest, suite: "CalcTest", test: "other()", message: "IllegalStateException: bad state"},
		{kind: KindBuild, file: "src/main/java/com/ex/Calc.java", line: 4, message: "';' expected"},
	})
}

func TestParseAutoDetectsFormats(t *testing.T) {
	// `make test` wrapping go test: the auto parser still finds the failures.
	got := ParseFailures(FrameworkAuto, "go test ./...\n"+readTestdata(t, "go_plain.txt")+"make: *** [Makefile:3: test] Error 1\n")
	if len(got) != len(goWant) {
		for _, f := range got {
			t.Logf("%+v", f)
		}
		t.Fatalf("auto parse found %d failures, want %d", len(got), len(goWant))
	}
}

func TestCleanOutputStripsEscapesAndCarriageReturns(t *testing.T) {
	in := "\x1b[31mFAIL\x1b[0m src/a.test.js\r\nprogress 10%\rprogress 100%\n\x1b]8;;http://x\x07link\x1b]8;;\x07"
	got := cleanOutput(in)
	if got != "FAIL src/a.test.js\nprogress 100%\nlink" {
		t.Fatalf("cleanOutput = %q", got)
	}
}

func TestFailureKeyIgnoresVolatileDetails(t *testing.T) {
	a := Failure{Kind: KindTest, Suite: "p", Test: "TestX", File: "x_test.go", Line: 10, Message: "got 1"}
	b := Failure{Kind: KindTest, Suite: "p", Test: "TestX", File: "x_test.go", Line: 12, Message: "got 2"}
	if a.Key() != b.Key() {
		t.Fatal("a test's key must not depend on its line or message")
	}
	e1 := Failure{Kind: KindError, Message: "crashed after 1.23s at 0xdeadbeef"}
	e2 := Failure{Kind: KindError, Message: "crashed after 4.56s at 0xcafe"}
	if e1.Key() != e2.Key() {
		t.Fatal("unattributed failures must hash without volatile numbers")
	}
	if e1.Key() == (Failure{Kind: KindError, Message: "other"}).Key() {
		t.Fatal("different messages must have different keys")
	}
}

func TestFormatFailuresOrdersAndBudgets(t *testing.T) {
	fs := []Failure{
		{Kind: KindTest, Test: "TestNoInfo"},
		{Kind: KindTest, Test: "TestLocated", File: "a_test.go", Line: 3, Message: "boom"},
		{Kind: KindBuild, File: "a.go", Line: 1, Message: "undefined: x"},
	}
	out := FormatFailures(fs, 10000)
	iBuild, iLoc, iNo := strings.Index(out, "[build] a.go:1"), strings.Index(out, "TestLocated"), strings.Index(out, "TestNoInfo")
	if iBuild < 0 || iLoc < 0 || iNo < 0 || !(iBuild < iLoc && iLoc < iNo) {
		t.Fatalf("failures not ordered by relevance:\n%s", out)
	}
	var many []Failure
	for i := range 200 {
		many = append(many, Failure{Kind: KindTest, Test: "TestCase" + strings.Repeat("x", 30) + strconv.Itoa(i), Message: strings.Repeat("detail ", 20)})
	}
	small := FormatFailures(many, 1500)
	if len(small) > 1700 || !strings.Contains(small, "more failures not shown") {
		t.Fatalf("budget not respected (%d bytes):\n%s", len(small), small)
	}
	tiny := FormatFailures(many[:1], 40)
	if tiny == "" || len(tiny) > 80 {
		t.Fatalf("a single oversized failure must still be shown, cut: %q", tiny)
	}
}

func TestFinishFailuresDedupesAndCaps(t *testing.T) {
	var in []Failure
	for i := 0; i < maxParsedFailures+50; i++ {
		in = append(in, Failure{Kind: KindTest, Test: "T" + strconv.Itoa(i%600)})
	}
	in = append(in, in[0])
	out := finishFailures(in)
	if len(out) != maxParsedFailures {
		t.Fatalf("got %d failures, want cap %d", len(out), maxParsedFailures)
	}
	long := finishFailures([]Failure{{Kind: KindError, Message: strings.Repeat("line\n", 100)}})
	if n := strings.Count(long[0].Message, "\n"); n > maxMessageLines+1 {
		t.Fatalf("message not trimmed: %d lines", n)
	}
}
