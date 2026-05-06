package main

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"time"
)

// handleConfigHTML serves a simple form view of the current askd
// runtime config, and accepts form-encoded POSTs to update it. This
// mirrors the cllm /config-html convenience endpoint at a much smaller
// scale — askd's config surface is small (just the bench defaults).
func (s *askdServer) handleConfigHTML(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderConfigForm(w, r, "")
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			s.renderConfigForm(w, r, "parse form: "+err.Error())
			return
		}
		spec := formToJobSpec(r.Form)
		s.cfg.update(spec)
		http.Redirect(w, r, "/config/html", http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *askdServer) renderConfigForm(w http.ResponseWriter, r *http.Request, errMsg string) {
	cfg := s.cfg.snapshot()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>askd config</title>
<style>body{font-family:system-ui,sans-serif;margin:2em;max-width:760px}
label{display:block;margin:.5em 0;font-weight:600}
input,select{width:100%%;padding:.4em;font:inherit}
.row{display:grid;grid-template-columns:1fr 1fr;gap:1em}
.err{color:#b00;margin:1em 0}
.note{color:#666;font-weight:400;font-size:.9em}
button{padding:.6em 1em;font:inherit}
.actions{margin-top:1em;display:flex;gap:.5em;flex-wrap:wrap}
</style></head><body>`)
	fmt.Fprintf(w, `<h1>askd config</h1><p class="note">Live runtime defaults. POST /config/reset to revert to startup values.</p>`)
	if errMsg != "" {
		fmt.Fprintf(w, `<div class="err">%s</div>`, html.EscapeString(errMsg))
	}
	fmt.Fprintf(w, `<form method="post" action="/config/html">`)

	intField := func(name, label string, v int) {
		fmt.Fprintf(w, `<label>%s<input type="number" name="%s" value="%d"></label>`,
			html.EscapeString(label), name, v)
	}
	floatField := func(name, label string, v float64) {
		fmt.Fprintf(w, `<label>%s<input type="number" step="any" name="%s" value="%s"></label>`,
			html.EscapeString(label), name, strconv.FormatFloat(v, 'f', -1, 64))
	}
	strField := func(name, label, v string) {
		fmt.Fprintf(w, `<label>%s<input type="text" name="%s" value="%s"></label>`,
			html.EscapeString(label), name, html.EscapeString(v))
	}
	boolField := func(name, label string, v bool) {
		checked := ""
		if v {
			checked = "checked"
		}
		fmt.Fprintf(w, `<label><input type="checkbox" name="%s" value="true" %s> %s</label>`,
			name, checked, html.EscapeString(label))
	}

	fmt.Fprintf(w, `<div class="row">`)
	intField("bench", "bench (concurrent workers)", cfg.bench)
	intField("count", "count (stop after N)", cfg.count)
	intField("duration_ms", "duration (ms; 0 = unbounded)", int(cfg.duration/time.Millisecond))
	intField("max_tokens", "max_tokens", cfg.maxTokens)
	intField("ramp_start", "ramp_start", cfg.rampStart)
	intField("ramp_end", "ramp_end", cfg.rampEnd)
	intField("ramp_duration_ms", "ramp_duration (ms)", int(cfg.rampDuration/time.Millisecond))
	floatField("temperature", "temperature", cfg.temperature)
	fmt.Fprintf(w, `</div>`)

	strField("url", "url", cfg.url)
	strField("model", "model (empty = autodetect)", cfg.model)
	strField("system", "system prompt", cfg.systemPrompt)
	strField("prompt", "default prompt", cfg.promptText)
	strField("dsl", "dsl directives", cfg.dsl)

	fmt.Fprintf(w, `<div class="row">`)
	boolField("loop", "loop prompt list", cfg.loop)
	boolField("random", "random prompt order", cfg.random)
	boolField("warmup", "warmup", cfg.warmup)
	boolField("stream", "stream", cfg.stream)
	boolField("quiet", "quiet", cfg.quiet)
	boolField("json", "json output", cfg.json)
	boolField("report", "report on completion", cfg.report)
	fmt.Fprintf(w, `</div>`)

	fmt.Fprintf(w, `<div class="actions">
<button type="submit">save</button>
<button type="button" onclick="fetch('/config/reset',{method:'POST'}).then(()=>location.reload())">reset to defaults</button>
<button type="button" onclick="fetch('/bench',{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'}).then(()=>location.href='/bench')">start bench</button>
<button type="button" onclick="fetch('/bench/pause',{method:'POST'})">pause</button>
<button type="button" onclick="fetch('/bench/start',{method:'POST'})">start/resume</button>
<button type="button" onclick="fetch('/bench/stop',{method:'POST'})">stop</button>
<button type="button" onclick="fetch('/bench/restart',{method:'POST'})">restart</button>
</div>`)
	fmt.Fprintf(w, `</form></body></html>`)
}

// formToJobSpec converts a form-encoded set of values into a jobSpec.
// Only fields whose form values are non-empty are populated; the rest
// are zero so runtimeConfig.update() preserves them. Booleans are
// special-cased: HTML checkboxes only submit when checked, so any
// missing boolean is treated as false (intentional unset).
func formToJobSpec(vals map[string][]string) jobSpec {
	getStr := func(k string) string {
		if v := vals[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	getInt := func(k string) int {
		s := getStr(k)
		if s == "" {
			return 0
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	getFloatPtr := func(k string) *float64 {
		s := getStr(k)
		if s == "" {
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		return &f
	}
	getBool := func(k string) bool {
		s := getStr(k)
		return s == "true" || s == "on" || s == "1"
	}
	getBoolPtr := func(k string) *bool {
		// Always present in the form (we render every checkbox), so
		// we can flip both directions. If absent (legacy form), nil.
		if _, ok := vals[k]; !ok {
			return nil
		}
		v := getBool(k)
		return &v
	}
	return jobSpec{
		Bench:        getInt("bench"),
		Count:        getInt("count"),
		DurationMs:   getInt("duration_ms"),
		RampStart:    getInt("ramp_start"),
		RampEnd:      getInt("ramp_end"),
		RampDurMs:    getInt("ramp_duration_ms"),
		MaxTokens:    getInt("max_tokens"),
		Temperature:  getFloatPtr("temperature"),
		URL:          getStr("url"),
		Model:        getStr("model"),
		SystemPrompt: getStr("system"),
		Prompt:       getStr("prompt"),
		DSL:          getStr("dsl"),
		Loop:         getBool("loop"),
		Random:       getBool("random"),
		Warmup:       getBool("warmup"),
		Stream:       getBoolPtr("stream"),
		Quiet:        getBool("quiet"),
		JSON:         getBool("json"),
		Report:       getBoolPtr("report"),
	}
}
