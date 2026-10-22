package web

import (
	"crypto/rand"
	"sync"
	"time"
)

const (
	JobRunning = "running"
	JobOK      = "ok"
	JobFail    = "fail"

	jobsKept    = 20
	outputLimit = 64 << 10
	truncated   = "\n… output truncated\n"
)

type Job struct {
	ID       string     `json:"id"`
	App      string     `json:"app"`
	Action   string     `json:"action"`
	Tag      string     `json:"tag"`
	State    string     `json:"state"`
	Code     int        `json:"code"`
	Output   string     `json:"output"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished"`
}

type jobs struct {
	mu   sync.Mutex
	list []*Job
}

type output struct {
	jobs *jobs
	job  *Job
}

func (o output) Write(p []byte) (int, error) {
	o.jobs.mu.Lock()
	defer o.jobs.mu.Unlock()
	switch room := outputLimit - len(o.job.Output); {
	case room <= 0:
	case len(p) > room:
		o.job.Output += string(p[:room]) + truncated
	default:
		o.job.Output += string(p)
	}
	return len(p), nil
}

func (j *jobs) start(app, action, tag string, now time.Time) (*Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, existing := range j.list {
		if existing.App == app && existing.State == JobRunning {
			return existing, false
		}
	}
	job := &Job{ID: rand.Text(), App: app, Action: action, Tag: tag, State: JobRunning, Started: now}
	j.list = append(j.list, job)
	j.trim()
	return job, true
}

func (j *jobs) trim() {
	for i := 0; len(j.list) > jobsKept && i < len(j.list); {
		if j.list[i].State == JobRunning {
			i++
			continue
		}
		j.list = append(j.list[:i], j.list[i+1:]...)
	}
}

func (j *jobs) finish(job *Job, code int, now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job.Code, job.Finished, job.State = code, &now, JobOK
	if code != 0 {
		job.State = JobFail
	}
}

func (j *jobs) get(id string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, job := range j.list {
		if job.ID == id {
			return *job, true
		}
	}
	return Job{}, false
}

func (j *jobs) all() []Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Job, 0, len(j.list))
	for _, job := range j.list {
		out = append(out, *job)
	}
	return out
}
