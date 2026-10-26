package hub

import "testing"

func TestPaths(t *testing.T) {
	p := Paths{Dir: "/opt/deploy-hub"}
	cases := map[string]string{
		p.AppsList():           "/opt/deploy-hub/apps.list",
		p.Journal():            "/opt/deploy-hub/deploys.log",
		p.Lock():               "/opt/deploy-hub/deploy.lock",
		p.Smoke():              "/opt/deploy-hub/smoke.conf",
		p.LastError():          "/opt/deploy-hub/last-error.log",
		p.Binary():             "/opt/deploy-hub/bin/deploy-hub",
		p.AppsRoot():           "/opt",
		p.LogFile("runner"):    "/opt/deploy-hub/logs/runner.log",
		p.CacheLock():          "/opt/deploy-hub/mutation-cache.lock",
		p.MutationCache("web"): "/opt/deploy-hub/mutation-cache/web",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %s want %s", got, want)
		}
	}
}
