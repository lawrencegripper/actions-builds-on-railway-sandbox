package dashboard

import (
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/lawrencegripper/actions-builds-on-railway-sandbox/internal/scaler"
)

// Snapshotter supplies the live runner counts and configured bounds (satisfied
// by *scaler.Scaler).
type Snapshotter interface {
	Snapshot() scaler.Counts
	MinRunners() int
	MaxRunners() int
}

// Server renders the dashboard page from a Store and a live counts source.
type Server struct {
	store         *Store
	snap          Snapshotter
	scaleSetName  string
	projectID     string
	environmentID string
	log           *slog.Logger
	tmpl          *template.Template
	buildTmpl     *template.Template
}

func NewServer(store *Store, snap Snapshotter, scaleSetName string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{
		store:         store,
		snap:          snap,
		scaleSetName:  scaleSetName,
		projectID:     os.Getenv("RAILWAY_PROJECT_ID"),
		environmentID: os.Getenv("RAILWAY_ENVIRONMENT_ID"),
		log:           log,
		tmpl:          template.Must(template.New("page").Funcs(tmplFuncs).Parse(pageHTML)),
		buildTmpl:     template.Must(template.New("build").Funcs(tmplFuncs).Parse(buildHTML)),
	}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/builds/", s.build)
	mux.HandleFunc("/", s.index)
	return mux
}

// sandboxURL builds a Railway dashboard deep link to a specific sandbox. Returns
// "" when the project/env ids are not injected (e.g. local runs), and the page
// falls back to plain text.
func (s *Server) sandboxURL(sandboxID string) string {
	if s.projectID == "" || s.environmentID == "" || sandboxID == "" {
		return ""
	}
	return "https://railway.com/project/" + s.projectID + "/sandboxes/" + sandboxID + "?environmentId=" + s.environmentID
}

// jobRow decorates a RecentJob with a resolved sandbox link for the template.
type jobRow struct {
	RecentJob
	SandboxURL string
}

type pageData struct {
	ScaleSetName string
	Counts       scaler.Counts
	Min          int
	Max          int
	Usage        Usage
	Jobs         []jobRow
	MaxDay       int
	Builds       []TemplateBuild
	Now          time.Time
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	usage, err := s.store.Usage(now)
	if err != nil {
		s.log.Error("usage query", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	builds, err := s.store.TemplateBuilds(now, 5)
	if err != nil {
		s.log.Error("builds query", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	jobs := make([]jobRow, 0, len(usage.Recent))
	for _, j := range usage.Recent {
		jobs = append(jobs, jobRow{RecentJob: j, SandboxURL: s.sandboxURL(j.SandboxID)})
	}
	data := pageData{
		ScaleSetName: s.scaleSetName,
		Counts:       s.snap.Snapshot(),
		Min:          s.snap.MinRunners(),
		Max:          s.snap.MaxRunners(),
		Usage:        usage,
		Jobs:         jobs,
		MaxDay:       maxDay(usage.PerDay),
		Builds:       builds,
		Now:          now,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		s.log.Error("render", slog.String("error", err.Error()))
	}
}

func (s *Server) build(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Path[len("/builds/"):], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	b, err := s.store.TemplateBuildByID(now, id)
	if err != nil {
		s.log.Error("build query", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.buildTmpl.Execute(w, struct {
		ScaleSetName string
		Build        *TemplateBuild
		Now          time.Time
	}{s.scaleSetName, b, now}); err != nil {
		s.log.Error("render build", slog.String("error", err.Error()))
	}
}

func maxDay(days []DayCount) int {
	m := 0
	for _, d := range days {
		if d.Count > m {
			m = d.Count
		}
	}
	return m
}

var tmplFuncs = template.FuncMap{
	"barHeight": func(count, max int) int {
		if max == 0 {
			return 0
		}
		h := count * 100 / max
		if h < 2 && count > 0 {
			h = 2
		}
		return h
	},
	"dur": func(d time.Duration) string {
		return d.Round(time.Second).String()
	},
	"shortDay": func(date string) string {
		t, err := time.Parse("2006-01-02", date)
		if err != nil {
			return date
		}
		return t.Format("Mon")
	},
	"clock": func(t time.Time) string { return t.Format("15:04:05 MST") },
	"stamp": func(t time.Time) string { return t.Format("Jan 2 15:04") },
	"buildPill": func(status string) string {
		switch status {
		case "READY":
			return "ok"
		case "FAILED":
			return "bad"
		default:
			return "run"
		}
	},
}

const styleBlock = `<style>
:root{--bg:#0d1117;--card:#161b22;--line:#30363d;--fg:#e6edf3;--dim:#8b949e;--accent:#7c3aed;--ok:#3fb950;--bad:#f85149;}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;padding:32px 20px}
.wrap{max-width:920px;margin:0 auto}
h1{font-size:20px;margin:0 0 2px;font-weight:600}
.sub{color:var(--dim);font-size:13px;margin:0 0 24px}
.grid{display:grid;gap:16px}
@media(min-width:720px){.cols{grid-template-columns:repeat(5,1fr)}}
.cols{grid-template-columns:repeat(2,1fr)}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px}
.stat .n{font-size:26px;font-weight:600;letter-spacing:-.5px}
.stat .l{color:var(--dim);font-size:12px;text-transform:uppercase;letter-spacing:.04em}
.stat .n.busy{color:var(--accent)}
h2{font-size:13px;text-transform:uppercase;letter-spacing:.05em;color:var(--dim);margin:32px 0 12px;font-weight:600}
.usage{display:grid;gap:16px}
@media(min-width:720px){.usage{grid-template-columns:1.4fr 1fr}}
.chart{display:flex;align-items:flex-end;gap:10px;height:140px;padding-top:8px}
.bar{flex:1;display:flex;flex-direction:column;align-items:center;gap:6px;height:100%;justify-content:flex-end}
.bar .fill{width:100%;background:linear-gradient(180deg,var(--accent),#5b21b6);border-radius:4px 4px 0 0;min-height:0}
.bar .c{font-size:12px;color:var(--fg)}
.bar .d{font-size:11px;color:var(--dim)}
.kv{display:flex;justify-content:space-between;padding:7px 0;border-bottom:1px solid var(--line)}
.kv:last-child{border-bottom:0}
.kv .k{color:var(--dim)}
table{width:100%;border-collapse:collapse;font-size:13px}
th{text-align:left;color:var(--dim);font-weight:500;font-size:11px;text-transform:uppercase;letter-spacing:.04em;padding:6px 8px;border-bottom:1px solid var(--line)}
td{padding:8px;border-bottom:1px solid var(--line)}
tr:last-child td{border-bottom:0}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.pill{display:inline-block;padding:2px 8px;border-radius:20px;font-size:12px}
.pill.ok{background:rgba(63,185,80,.15);color:var(--ok)}
.pill.bad{background:rgba(248,81,73,.15);color:var(--bad)}
.pill.run{background:rgba(124,58,237,.18);color:#b794f6}
.empty{color:var(--dim);padding:16px 8px}
a.link{color:#b794f6;text-decoration:none}
a.link:hover{text-decoration:underline}
pre{background:#0b0f14;border:1px solid var(--line);border-radius:8px;padding:12px;overflow:auto;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;color:var(--fg);white-space:pre-wrap;word-break:break-word}
.note{color:var(--dim);font-size:12px;margin:8px 0}
footer{color:var(--dim);font-size:12px;margin-top:28px;text-align:center}
</style>`

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="15">
<title>{{.ScaleSetName}} · runners</title>
` + styleBlock + `
</head>
<body>
<div class="wrap">
<h1>{{.ScaleSetName}}</h1>
<p class="sub">Railway sandbox runners · {{clock .Now}}</p>

<div class="grid cols">
<div class="card stat"><div class="n">{{.Counts.Total}}</div><div class="l">Total</div></div>
<div class="card stat"><div class="n">{{.Counts.Provisioning}}</div><div class="l">Provisioning</div></div>
<div class="card stat"><div class="n">{{.Counts.Idle}}</div><div class="l">Idle</div></div>
<div class="card stat"><div class="n busy">{{.Counts.Busy}}</div><div class="l">Busy</div></div>
<div class="card stat"><div class="n">{{.Min}}–{{.Max}}</div><div class="l">Min–Max</div></div>
</div>

<h2>Usage · last 7 days</h2>
<div class="usage">
<div class="card">
<div class="chart">
{{range .Usage.PerDay}}
<div class="bar">
<div class="c">{{.Count}}</div>
<div class="fill" style="height:{{barHeight .Count $.MaxDay}}%"></div>
<div class="d">{{shortDay .Date}}</div>
</div>
{{end}}
</div>
</div>
<div class="card">
<div class="kv"><span class="k">Jobs run</span><span>{{.Usage.Total}}</span></div>
<div class="kv"><span class="k">Succeeded</span><span>{{.Usage.Succeeded}}</span></div>
<div class="kv"><span class="k">Failed / cancelled</span><span>{{.Usage.Failed}}</span></div>
<div class="kv"><span class="k">Avg duration</span><span>{{dur .Usage.AvgDuration}}</span></div>
<div class="kv"><span class="k">Total duration</span><span>{{dur .Usage.TotalDuration}}</span></div>
</div>
</div>

<h2>Image builds</h2>
<div class="card">
{{if .Builds}}
<table>
<thead><tr><th>Runner version</th><th>Started</th><th>Duration</th><th>Status</th><th></th></tr></thead>
<tbody>
{{range .Builds}}
<tr>
<td class="mono">{{if .RunnerVersion}}{{.RunnerVersion}}{{else}}—{{end}}</td>
<td>{{stamp .Started}}</td>
<td>{{if .Running}}—{{else}}{{dur .Duration}}{{end}}</td>
<td><span class="pill {{buildPill .Status}}">{{.Status}}</span></td>
<td><a class="link" href="/builds/{{.ID}}">details</a></td>
</tr>
{{end}}
</tbody>
</table>
{{else}}
<div class="empty">No image builds recorded yet.</div>
{{end}}
</div>

<h2>Recent jobs</h2>
<div class="card">
{{if .Jobs}}
<table>
<thead><tr><th>Provider</th><th>Job</th><th>Runner</th><th>Sandbox</th><th>Started</th><th>Duration</th><th>Result</th></tr></thead>
<tbody>
{{range .Jobs}}
<tr>
<td>{{if .Provider}}{{.Provider}}{{else}}—{{end}}</td>
<td>{{if .JobName}}{{.JobName}}{{else if .ExternalJobID}}#{{.ExternalJobID}}{{else}}—{{end}}</td>
<td class="mono">{{.RunnerName}}</td>
<td class="mono">
{{if .SandboxID}}
{{if .SandboxURL}}<a class="link" href="{{.SandboxURL}}" target="_blank" rel="noreferrer">{{.SandboxID}}</a>
{{else}}{{.SandboxID}}{{end}}
{{else}}—{{end}}
</td>
<td>{{stamp .Started}}</td>
<td>{{if .Running}}—{{else}}{{dur .Duration}}{{end}}</td>
<td>
{{if .Running}}<span class="pill run">running</span>
{{else if eq .Result "succeeded"}}<span class="pill ok">{{.Result}}</span>
{{else}}<span class="pill bad">{{.Result}}</span>{{end}}
</td>
</tr>
{{end}}
</tbody>
</table>
{{else}}
<div class="empty">No jobs recorded yet.</div>
{{end}}
</div>

<footer>auto-refreshes every 15s</footer>
</div>
</body>
</html>`

const buildHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.ScaleSetName}} · template build</title>
` + styleBlock + `
</head>
<body>
<div class="wrap">
<h1>Template build</h1>
<p class="sub"><a class="link" href="/">← {{.ScaleSetName}}</a> · {{clock .Now}}</p>

<div class="card">
<div class="kv"><span class="k">Status</span><span class="pill {{buildPill .Build.Status}}">{{.Build.Status}}</span></div>
<div class="kv"><span class="k">Runner version</span><span class="mono">{{if .Build.RunnerVersion}}{{.Build.RunnerVersion}}{{else}}—{{end}}</span></div>
<div class="kv"><span class="k">Build id</span><span class="mono">{{if .Build.BuildID}}{{.Build.BuildID}}{{else}}—{{end}}</span></div>
<div class="kv"><span class="k">Started</span><span>{{stamp .Build.Started}}</span></div>
<div class="kv"><span class="k">Duration</span><span>{{if .Build.Running}}running{{else}}{{dur .Build.Duration}}{{end}}</span></div>
</div>

{{if .Build.Error}}
<h2>Error</h2>
<div class="card"><pre>{{.Build.Error}}</pre></div>
{{end}}

<h2>Recipe</h2>
<div class="card"><pre>{{.Build.Recipe}}</pre></div>

<h2>Build logs</h2>
<div class="card">
{{if .Build.Logs}}<pre>{{.Build.Logs}}</pre>
{{else}}<p class="note">Railway's API does not expose per-line template build logs. The SandboxTemplateBuild resource carries only a content-addressed id and a status; there is no logs field or a deployment id to query buildLogs against. Status, the error above, the recipe, and timing are recorded instead.</p>{{end}}
</div>

<footer>actions-builds-on-railway-sandbox</footer>
</div>
</body>
</html>`
