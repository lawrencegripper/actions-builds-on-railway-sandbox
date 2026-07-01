package dashboard

import (
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/lawrencegripper/railway-sandbox-github-actions-runner/internal/scaler"
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
	store        *Store
	snap         Snapshotter
	scaleSetName string
	log          *slog.Logger
	tmpl         *template.Template
}

func NewServer(store *Store, snap Snapshotter, scaleSetName string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{
		store:        store,
		snap:         snap,
		scaleSetName: scaleSetName,
		log:          log,
		tmpl:         template.Must(template.New("page").Funcs(tmplFuncs).Parse(pageHTML)),
	}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", s.index)
	return mux
}

type pageData struct {
	ScaleSetName string
	Counts       scaler.Counts
	Min          int
	Max          int
	Usage        Usage
	MaxDay       int
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
	data := pageData{
		ScaleSetName: s.scaleSetName,
		Counts:       s.snap.Snapshot(),
		Min:          s.snap.MinRunners(),
		Max:          s.snap.MaxRunners(),
		Usage:        usage,
		MaxDay:       maxDay(usage.PerDay),
		Now:          now,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		s.log.Error("render", slog.String("error", err.Error()))
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
}

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="15">
<title>{{.ScaleSetName}} · runners</title>
<style>
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
footer{color:var(--dim);font-size:12px;margin-top:28px;text-align:center}
</style>
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

<h2>Recent jobs</h2>
<div class="card">
{{if .Usage.Recent}}
<table>
<thead><tr><th>Runner</th><th>Started</th><th>Duration</th><th>Result</th></tr></thead>
<tbody>
{{range .Usage.Recent}}
<tr>
<td class="mono">{{.RunnerName}}</td>
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
