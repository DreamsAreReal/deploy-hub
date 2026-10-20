package junit

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	ValueTests   = "tests"
	ValueFailed  = "failed"
	ValueSkipped = "skipped"
	reasonTests  = "tests failed or were skipped"
	reasonNone   = "no tests ran"
	failedForm   = "%s failed: %s"
	skippedForm  = "%s skipped: %s"
)

type outcomeText struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

type Case struct {
	Classname string       `xml:"classname,attr"`
	Name      string       `xml:"name,attr"`
	File      string       `xml:"file,attr"`
	Time      float64      `xml:"time,attr"`
	Failure   *outcomeText `xml:"failure"`
	Error     *outcomeText `xml:"error"`
	Skipped   *outcomeText `xml:"skipped"`
}

func (c Case) Passed() bool {
	return c.Failure == nil && c.Error == nil && c.Skipped == nil
}

type suite struct {
	Cases  []Case  `xml:"testcase"`
	Suites []suite `xml:"testsuite"`
}

func (s suite) all() []Case {
	out := append([]Case(nil), s.Cases...)
	for _, child := range s.Suites {
		out = append(out, child.all()...)
	}
	return out
}

func Parse(data []byte) ([]Case, error) {
	var root suite
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("junit report: %w", err)
	}
	return root.all(), nil
}

func Load(path string) ([]Case, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func Judge(cases []Case, id func(Case) string) gate.Outcome {
	var findings []string
	failed, skipped := 0, 0
	for _, c := range cases {
		switch {
		case c.Failure != nil || c.Error != nil:
			failed++
			findings = append(findings, fmt.Sprintf(failedForm, id(c), firstLine(c.Failure, c.Error)))
		case c.Skipped != nil:
			skipped++
			findings = append(findings, fmt.Sprintf(skippedForm, id(c), c.Skipped.Message))
		}
	}
	values := map[string]float64{ValueTests: float64(len(cases)), ValueFailed: float64(failed), ValueSkipped: float64(skipped)}
	if len(cases) == 0 {
		return gate.Judge(false, reasonNone, values, nil)
	}
	return gate.Judge(len(findings) == 0, reasonTests, values, findings)
}

func firstLine(outcomes ...*outcomeText) string {
	for _, o := range outcomes {
		if o != nil {
			return o.Message
		}
	}
	return ""
}

func Passed(cases []Case, id func(Case) string) map[string]bool {
	out := map[string]bool{}
	for _, c := range cases {
		if c.Passed() {
			out[id(c)] = true
		}
	}
	return out
}

func Seconds(cases []Case) float64 {
	total := 0.0
	for _, c := range cases {
		total += c.Time
	}
	return total
}
