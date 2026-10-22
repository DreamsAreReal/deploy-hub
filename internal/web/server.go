package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/DreamsAreReal/deploy-hub/internal/journal"
	"github.com/DreamsAreReal/deploy-hub/internal/registry"
	"github.com/DreamsAreReal/deploy-hub/internal/runner"
	"github.com/DreamsAreReal/deploy-hub/internal/validate"
)

const (
	ActionRollback = "rollback"
	ActionRedeploy = "redeploy"

	bodyLimit        = 4 << 10
	dashboardRefresh = 30
	jobRefresh       = 2
	contentJSON      = "application/json"
	contentForm      = "application/x-www-form-urlencoded"
	contentHTML      = "text/html; charset=utf-8"
	headerCSP        = "default-src 'none'; img-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
	fetchSameOrigin  = "same-origin"
	fieldApp         = "app"
	fieldAction      = "action"
	fieldTag         = "tag"
	jobPath          = "/actions/"
)

var (
	//go:embed static
	assets embed.FS
	//go:embed templates
	layouts embed.FS

	pages = template.Must(template.ParseFS(layouts, "templates/*.gohtml"))

	errBadAction  = errors.New("no such action: only rollback or redeploy")
	errBadApp     = errors.New("no such app")
	errBadTag     = errors.New("redeploy needs a sha-… version, rollback takes none")
	errBadOrigin  = errors.New("the request came from another site and was refused")
	errBadType    = errors.New("actions start only from the button on the page")
	errBadBody    = errors.New("the form arrived broken")
	errNoJob      = errors.New("no such run: the server may have restarted")
	errNoPrevious = errors.New("the app has no previous version to roll back to")
	errNoCurrent  = errors.New("the app was never deployed, nothing to redeploy")
	errUnreadable = errors.New("could not read the app list on the server")
)

type Actor func(ctx context.Context, request string, out io.Writer) int

type Server struct {
	Snapshot func(ctx context.Context) ([]runner.AppStatus, error)
	Journal  func() ([]journal.Entry, error)
	Act      Actor
	Now      func() time.Time
	Log      *slog.Logger
	HostSlug string
	jobs     jobs
	running  sync.WaitGroup
}

type actionRequest struct {
	App    string
	Action string
	Tag    string
}

type page struct {
	Title   string
	Refresh int
	Clock   string
	Body    any
}

type confirmView struct {
	actionRequest
	Question string
	Explain  string
	Button   string
}

type jobView struct {
	Job
	Face  string
	Tone  string
	Title string
	Text  string
	Party bool
}

func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.dashboard)
	mux.HandleFunc("GET /api/overview", s.overviewJSON)
	mux.HandleFunc("GET /confirm/{action}/{app}", s.confirm)
	mux.HandleFunc("POST /actions", s.startAction)
	mux.HandleFunc("GET "+jobPath+"{id}", s.action)
	return secure(mux)
}

func (s *Server) Wait() {
	s.running.Wait()
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", headerCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) overview(ctx context.Context) (Overview, error) {
	statuses, err := s.Snapshot(ctx)
	if err != nil {
		s.Log.Error("apps.list unreadable", "error", err.Error())
		return Overview{}, errUnreadable
	}
	entries, err := s.Journal()
	if err != nil {
		s.Log.Warn("journal read failed", "error", err.Error())
	}
	o := buildOverview(s.Now(), s.HostSlug, statuses, entries)
	o.Jobs = s.jobs.all()
	return o, nil
}

func (s *Server) overviewJSON(w http.ResponseWriter, r *http.Request) {
	o, err := s.overview(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	o, err := s.overview(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	running := map[string]string{}
	for _, j := range o.Jobs {
		if j.State == JobRunning {
			running[j.App] = j.ID
		}
	}
	v := dashboard(o, running)
	s.render(w, http.StatusOK, "dashboard", page{Title: "Apps", Refresh: dashboardRefresh, Clock: v.Clock, Body: v})
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	req := actionRequest{App: r.PathValue(fieldApp), Action: r.PathValue(fieldAction)}
	if err := req.validateTarget(); err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	app, err := s.find(r.Context(), req.App)
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	v, err := confirmFor(req, app)
	if err != nil {
		s.fail(w, http.StatusConflict, err)
		return
	}
	s.render(w, http.StatusOK, "confirm", page{Title: v.Question, Body: v})
}

func (s *Server) find(ctx context.Context, name string) (runner.AppStatus, error) {
	statuses, err := s.Snapshot(ctx)
	if err != nil {
		return runner.AppStatus{}, errUnreadable
	}
	for _, st := range statuses {
		if st.Name == name {
			return st, nil
		}
	}
	return runner.AppStatus{}, errBadApp
}

func confirmFor(req actionRequest, app runner.AppStatus) (confirmView, error) {
	if req.Action == ActionRollback {
		if app.Previous == "" {
			return confirmView{}, errNoPrevious
		}
		return confirmView{actionRequest: req, Question: "Roll back " + app.Name + "?", Button: "↩️ Yes, roll back",
			Explain: "The previous version " + registry.Sha7(app.Previous) + " comes back. It starts next to the live one and must pass the health check before it gets traffic."}, nil
	}
	if app.Current == "" {
		return confirmView{}, errNoCurrent
	}
	req.Tag = app.Current
	return confirmView{actionRequest: req, Question: "Redeploy " + app.Name + "?", Button: "🔁 Yes, redeploy",
		Explain: "The current version " + registry.Sha7(app.Current) + " is recreated through the same health check. If the new copy does not come up, traffic stays on the old one."}, nil
}

func (s *Server) startAction(w http.ResponseWriter, r *http.Request) {
	req, status, err := decodeAction(r)
	if err != nil {
		s.Log.Warn("action refused", "error", err.Error())
		s.fail(w, status, err)
		return
	}
	job, started := s.jobs.start(req.App, req.Action, req.Tag, s.Now())
	if started {
		s.Log.Info("action started", "id", job.ID, "app", req.App, "action", req.Action, "tag", req.Tag)
		s.running.Add(1)
		go s.run(context.WithoutCancel(r.Context()), job, req.command())
	}
	http.Redirect(w, r, jobPath+job.ID, http.StatusSeeOther)
}

func (s *Server) run(ctx context.Context, job *Job, request string) {
	defer s.running.Done()
	code := s.Act(ctx, request, output{jobs: &s.jobs, job: job})
	s.jobs.finish(job, code, s.Now())
	s.Log.Info("action finished", "id", job.ID, "app", job.App, "action", job.Action, "exit_code", code)
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.get(r.PathValue("id"))
	if !ok {
		s.fail(w, http.StatusNotFound, errNoJob)
		return
	}
	v := describe(job)
	p := page{Title: v.Title, Body: v}
	if job.State == JobRunning {
		p.Refresh = jobRefresh
	}
	s.render(w, http.StatusOK, "job", p)
}

func describe(job Job) jobView {
	v := jobView{Job: job}
	switch job.State {
	case JobRunning:
		verb := "Rolling back "
		if job.Action == ActionRedeploy {
			verb = "Redeploying "
		}
		v.Face, v.Tone, v.Title, v.Text = "⚙️", ToneBusy, verb+job.App+"…", "You can close this page: the work keeps running on the server."
	case JobOK:
		v.Face, v.Tone, v.Title, v.Text, v.Party = "🥳", ToneGood, "Done!", job.App+" passed the health check and is answering.", true
	default:
		v.Face, v.Tone, v.Title, v.Text = "😿", ToneCritical, "That did not work", "The log below shows what went wrong."
	}
	return v
}

func decodeAction(r *http.Request) (actionRequest, int, error) {
	if !sameOrigin(r) {
		return actionRequest{}, http.StatusForbidden, errBadOrigin
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != contentForm {
		return actionRequest{}, http.StatusUnsupportedMediaType, errBadType
	}
	r.Body = http.MaxBytesReader(nil, r.Body, bodyLimit)
	if err := r.ParseForm(); err != nil {
		return actionRequest{}, http.StatusBadRequest, errBadBody
	}
	req := actionRequest{App: r.PostForm.Get(fieldApp), Action: r.PostForm.Get(fieldAction), Tag: r.PostForm.Get(fieldTag)}
	return req, http.StatusBadRequest, req.validate()
}

func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != fetchSameOrigin {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (a actionRequest) validateTarget() error {
	if validate.AppName(a.App) != nil {
		return errBadApp
	}
	if a.Action != ActionRollback && a.Action != ActionRedeploy {
		return errBadAction
	}
	return nil
}

func (a actionRequest) validate() error {
	if err := a.validateTarget(); err != nil {
		return err
	}
	if a.Action == ActionRollback && a.Tag != "" || a.Action == ActionRedeploy && validate.Tag(a.Tag) != nil {
		return errBadTag
	}
	return nil
}

func (a actionRequest) command() string {
	if a.Action == ActionRedeploy {
		return runner.VerbRollback + " " + a.App + " " + a.Tag
	}
	return runner.VerbRollback + " " + a.App
}

func (s *Server) fail(w http.ResponseWriter, status int, err error) {
	s.render(w, status, "error", page{Title: "Error", Body: err.Error()})
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, p); err != nil {
		s.Log.Error("page render failed", "page", name, "error", err.Error())
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentHTML)
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
