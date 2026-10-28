package stage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/gate"
)

const (
	ValueAlerts     = "alerts"
	zapVolumeSuffix = "-zap"
	zapMount        = "/zap/wrk"
	zapReport       = "zap.json"
	zapFullScan     = "zap-full-scan.py"
	zapAPIScan      = "zap-api-scan.py"
	zapRootUser     = "0"
	zapOwner        = "zap:zap"
	zapFormatOpenAP = "openapi"
	zapScanCapFmt   = "-config scanner.maxScanDurationInMins=%d"
	reasonDAST      = "dynamic scanning found vulnerabilities"
	reasonCrashed   = "the app stopped during the scan"
	alertFormat     = "%s %s (plugin %s, %s instances) %s"
	purposeDAST     = "dast"
	minRisk         = 1
	zapArgCount     = 20
)

var riskNames = map[string]string{"1": "Low", "2": "Medium", "3": "High"}

type zapAlert struct {
	PluginID  string `json:"pluginid"`
	Name      string `json:"name"`
	RiskCode  string `json:"riskcode"`
	Count     string `json:"count"`
	Instances []struct {
		URI string `json:"uri"`
	} `json:"instances"`
}

type zapReportDoc struct {
	Site []struct {
		Alerts []zapAlert `json:"alerts"`
	} `json:"site"`
}

func DAST(ctx context.Context, ws gate.Workspace) gate.Outcome {
	if ws.Image == "" {
		return gate.Fail(reasonNoImage)
	}
	st, err := Start(ctx, ws, purposeDAST)
	if err != nil {
		return gate.Fail(err.Error())
	}
	defer st.Stop(ctx)
	vol := st.Name + zapVolumeSuffix
	_, _ = ws.Run(ctx, DockerBinary, "volume", "rm", "-f", vol)
	if verr := st.prepareVolume(ctx, vol); verr != nil {
		return gate.Fail(verr.Error())
	}
	defer func() { _, _ = ws.Run(context.WithoutCancel(ctx), DockerBinary, "volume", "rm", "-f", vol) }()
	res, scanErr := ws.Run(ctx, DockerBinary, st.zapArgs(vol)...)
	report, err := st.readReport(ctx, vol)
	if err != nil {
		return gate.Fail(fmt.Sprintf("ZAP produced no report: %v: %s", scanErr, gate.Tail(res.Combined())))
	}
	return st.judge(ctx, report)
}

func (s *Stage) prepareVolume(ctx context.Context, vol string) error {
	if err := s.docker(ctx, "volume", "create", vol); err != nil {
		return err
	}
	return s.docker(ctx, "run", "--rm", "-u", zapRootUser, "-v", vol+":"+zapMount, "--entrypoint", "chown", s.ws.Policy.Images.ZAP, zapOwner, zapMount)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func minutes(d time.Duration) int {
	return int(math.Ceil(d.Minutes()))
}

func (s *Stage) zapArgs(vol string) []string {
	d := s.ws.App.DAST
	script, target := zapFullScan, s.InternalURL(firstNonEmpty(d.Target, s.ws.App.Health))
	var extra []string
	if d.OpenAPI != "" {
		script, target, extra = zapAPIScan, s.InternalURL(d.OpenAPI), []string{"-f", zapFormatOpenAP}
	}
	args := make([]string, 0, zapArgCount+len(extra))
	args = append(args,
		"run", "--rm", flagNetwork, s.Network, "-v", vol+":"+zapMount, s.ws.Policy.Images.ZAP,
		script, "-t", target, "-J", zapReport, "-I",
		"-m", strconv.Itoa(minutes(d.Spider)), "-T", strconv.Itoa(minutes(d.MaxTime)),
		"-z", fmt.Sprintf(zapScanCapFmt, minutes(d.MaxTime)),
	)
	return append(args, extra...)
}

func (s *Stage) readReport(ctx context.Context, vol string) (zapReportDoc, error) {
	res, err := s.ws.Run(ctx, DockerBinary, "run", "--rm", "-v", vol+":"+zapMount, "--entrypoint", "cat", s.ws.Policy.Images.ZAP, zapMount+"/"+zapReport)
	if err != nil {
		return zapReportDoc{}, err
	}
	var doc zapReportDoc
	return doc, json.Unmarshal([]byte(res.Stdout), &doc)
}

func (doc zapReportDoc) findings() []string {
	var out []string
	for _, site := range doc.Site {
		for _, a := range site.Alerts {
			if risk, _ := strconv.Atoi(a.RiskCode); risk >= minRisk {
				out = append(out, a.describe())
			}
		}
	}
	return out
}

func (a zapAlert) describe() string {
	uri := ""
	if len(a.Instances) > 0 {
		uri = a.Instances[0].URI
	}
	return fmt.Sprintf(alertFormat, riskNames[a.RiskCode], a.Name, a.PluginID, a.Count, uri)
}

func (s *Stage) judge(ctx context.Context, doc zapReportDoc) gate.Outcome {
	findings := doc.findings()
	state, err := s.State(ctx)
	if err != nil || !state.Running || state.OOM {
		findings = append(findings, reasonCrashed)
	}
	values := map[string]float64{ValueAlerts: float64(len(findings))}
	return gate.Judge(len(findings) == 0, reasonDAST, values, findings)
}
