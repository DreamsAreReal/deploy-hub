package junit

import (
	"path/filepath"
	"testing"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
	"github.com/DreamsAreReal/deploy-hub/internal/testutil"
)

const report = `<?xml version="1.0" encoding="utf-8"?>
<testsuites>
  <testsuite name="a">
    <testcase classname="tests.test_a" name="test_ok" file="tests/test_a.py" time="0.5"/>
    <testcase classname="tests.test_a" name="test_bad" time="0.25"><failure message="assert 1 == 2">trace</failure></testcase>
    <testsuite name="nested">
      <testcase classname="tests.test_b" name="test_crash" time="0.25"><error message="boom"/></testcase>
      <testcase classname="tests.test_b" name="test_later"><skipped message="not yet"/></testcase>
    </testsuite>
  </testsuite>
</testsuites>`

func name(c Case) string { return c.Classname + "." + c.Name }

func TestParseAndJudge(t *testing.T) {
	cases, err := Parse([]byte(report))
	testutil.NoErr(t, err)
	testutil.Equal(t, len(cases), 4)
	testutil.Equal(t, cases[0].File, "tests/test_a.py")
	out := Judge(cases, name)
	testutil.Equal(t, out.Status, gate.StatusFail)
	testutil.Equal(t, out.Reason, reasonTests)
	testutil.Equal(t, out.Findings, []string{
		"tests.test_a.test_bad failed: assert 1 == 2",
		"tests.test_b.test_crash failed: boom",
		"tests.test_b.test_later skipped: not yet",
	})
	testutil.Equal(t, out.Values, map[string]float64{ValueTests: 4, ValueFailed: 2, ValueSkipped: 1})
	testutil.Equal(t, Passed(cases, name), map[string]bool{"tests.test_a.test_ok": true})
	testutil.Equal(t, Seconds(cases), 1.0)
	testutil.Equal(t, Judge(cases[:1], name).Status, gate.StatusPass)
	empty := Judge(nil, name)
	testutil.Equal(t, empty.Reason, reasonNone)
	testutil.Equal(t, empty.Status, gate.StatusFail)
	testutil.Equal(t, firstLine(nil, nil), "")
}

func TestRootSuiteAndErrors(t *testing.T) {
	cases, err := Parse([]byte(`<testsuite><testcase classname="X" name="y"/></testsuite>`))
	testutil.NoErr(t, err)
	testutil.Equal(t, len(cases), 1)
	_, err = Parse([]byte("<testsuite>"))
	testutil.Fails(t, err)
	path := filepath.Join(t.TempDir(), "junit.xml")
	_, err = Load(path)
	testutil.Fails(t, err)
	testutil.Write(t, path, report)
	loaded, err := Load(path)
	testutil.NoErr(t, err)
	testutil.Equal(t, len(loaded), 4)
}
