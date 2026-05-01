package httpapi

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"

	"cllm/internal/runtimeconfig"
)

// configFormState carries everything the template needs to render either
// the read-only view or the edit form. When Values is non-nil (e.g. after a
// failed POST validation) those values are echoed back into the form so the
// user does not lose their edits; otherwise the live runtime values are used.
type configFormState struct {
	Edit   bool
	Values url.Values
	Error  string
	Status int
}

// configFieldKind drives input rendering.
type configFieldKind int

const (
	fieldInt configFieldKind = iota
	fieldFloat
	fieldString
	fieldSelect
	fieldPassword
)

// configField describes a single form field.
type configField struct {
	Key      string // canonical (snake_case) form/query name
	Label    string
	Kind     configFieldKind
	Help     string
	Min      string
	Max      string
	Step     string
	Default  string
	Current  string
	Options  []string // for fieldSelect (does not include the empty option)
	Optional bool     // empty value clears (e.g. dsl_default_profile)
}

// configSection groups related fields under a heading.
type configSection struct {
	Title  string
	Fields []configField
}

// configTemplateData is the root struct passed to the template.
type configTemplateData struct {
	Edit       bool
	Error      string
	ReadOnly   []configField
	Sections   []configSection
	APIExample string
}

func (h *Handler) renderConfigHTML(w http.ResponseWriter, r *http.Request, state configFormState) {
	cfg := h.currentConfig()
	profileNames := h.DSLProfileNames()

	get := func(canonical, hyphen string) string {
		if state.Values == nil {
			return ""
		}
		if v := state.Values.Get(canonical); v != "" {
			return v
		}
		return state.Values.Get(hyphen)
	}

	// pick chooses the user-supplied form value (when re-rendering after a
	// validation error) over the live current value.
	pickStr := func(canonical, hyphen, current string) string {
		if v := get(canonical, hyphen); v != "" {
			return v
		}
		return current
	}
	pickInt := func(canonical, hyphen string, current int) string {
		if v := get(canonical, hyphen); v != "" {
			return v
		}
		return strconv.Itoa(current)
	}
	pickFloat := func(canonical, hyphen string, current float64) string {
		if v := get(canonical, hyphen); v != "" {
			return v
		}
		return strconv.FormatFloat(current, 'f', -1, 64)
	}

	// Read-only fields go first.
	readOnly := []configField{
		{Key: "version", Label: "Version", Current: cfg.Version},
		{Key: "tokens_in_flight", Label: "Tokens In Flight", Current: strconv.FormatInt(cfg.TokensInFlight, 10)},
		{Key: "waiting_requests", Label: "Waiting Requests", Current: strconv.Itoa(cfg.WaitingRequests)},
		{Key: "cache_entries", Label: "Cache Entries", Current: strconv.Itoa(cfg.CacheEntries)},
	}

	sections := []configSection{
		{
			Title: "Cache",
			Fields: []configField{
				{
					Key: "cache_size", Label: "cache_size", Kind: fieldInt,
					Help:    "Maximum number of cached chat responses.",
					Min:     strconv.Itoa(runtimeconfig.MinCacheSize),
					Max:     strconv.Itoa(runtimeconfig.MaxCacheSize),
					Default: strconv.Itoa(runtimeconfig.DefaultCacheSize),
					Current: pickInt("cache_size", "cache-size", cfg.CacheSize),
				},
			},
		},
		{
			Title: "Downstream",
			Fields: []configField{
				{
					Key: "downstream_url", Label: "downstream_url", Kind: fieldString,
					Help:    "Downstream Chat Completions API base URL.",
					Default: runtimeconfig.DefaultDownstreamURL,
					Current: pickStr("downstream_url", "downstream-url", cfg.DownstreamURL),
				},
				{
					Key: "downstream_model", Label: "downstream_model", Kind: fieldString,
					Help:    "Default downstream model when the request omits one.",
					Current: pickStr("downstream_model", "downstream-model", cfg.DownstreamModel),
				},
				{
					Key: "downstream_token", Label: "downstream_token", Kind: fieldPassword,
					Help:    "Bearer token sent with downstream requests. Leave blank to keep the current value.",
					Current: "",
				},
			},
		},
		{
			Title: "Request Defaults",
			Fields: []configField{
				{
					Key: "system_prompt", Label: "system_prompt", Kind: fieldString,
					Help:    "Default system prompt for chat completions.",
					Default: runtimeconfig.DefaultSystemPrompt,
					Current: pickStr("system_prompt", "system-prompt", cfg.SystemPrompt),
				},
				{
					Key: "max_tokens", Label: "max_tokens", Kind: fieldInt,
					Help:    "Default maximum completion tokens per request.",
					Min:     strconv.Itoa(runtimeconfig.MinMaxTokens),
					Max:     strconv.Itoa(runtimeconfig.MaxMaxTokens),
					Default: strconv.Itoa(runtimeconfig.DefaultMaxTokens),
					Current: pickInt("max_tokens", "max-tokens", cfg.MaxTokens),
				},
				{
					Key: "temperature", Label: "temperature", Kind: fieldFloat,
					Help:    "Default temperature for chat completions.",
					Min:     "0", Max: "2", Step: "0.1",
					Default: strconv.FormatFloat(runtimeconfig.DefaultTemperature, 'f', -1, 64),
					Current: pickFloat("temperature", "temperature", cfg.Temperature),
				},
			},
		},
		// Throughput Limits, Prefill Simulation, and Stream Realism are
		// managed per-node in configs/nodes.yaml (capacity, prefill_*,
		// stream_* on Node/Class). The legacy global knobs they used to
		// edit no longer bind in multi-node deployments — every routed
		// node owns its own values — so they are hidden from /config to
		// avoid the appearance of control. The underlying handler fields
		// and CLI flags remain for the implicit single-node fallback.
		{
			Title: "DSL",
			Fields: []configField{
				{
					Key: "dsl_profile", Label: "dsl_default_profile", Kind: fieldSelect,
					Help:     "Default DSL profile applied to requests that omit `:dsl`. Empty = none.",
					Options:  profileNames,
					Optional: true,
					Current:  pickStr("dsl_profile", "dsl-profile", cfg.DSLDefaultProfile),
				},
			},
		},
	}

	data := configTemplateData{
		Edit:       state.Edit,
		Error:      state.Error,
		ReadOnly:   readOnly,
		Sections:   sections,
		APIExample: "GET /config?max_tokens=1000  \u00b7  curl -H 'Accept: application/json' /config",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	status := state.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if err := configTmpl.Execute(w, data); err != nil {
		// Headers already flushed; best we can do is log via the response writer.
		_, _ = fmt.Fprintf(w, "\n<!-- template error: %s -->\n", template.HTMLEscapeString(err.Error()))
	}
}

var configTmpl = template.Must(template.New("config").Parse(configTemplateHTML))

const configTemplateHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>cllm /config</title>
<style>
:root { color-scheme: light dark; }
body { font: 14px/1.45 system-ui, -apple-system, Segoe UI, sans-serif; margin: 1.5rem; max-width: 920px; }
h1 { margin: 0 0 0.25rem; font-size: 1.4rem; }
h2 { margin: 1.25rem 0 0.4rem; font-size: 1.05rem; border-bottom: 1px solid #8884; padding-bottom: 0.2rem; }
.subtitle { color: #888; margin-bottom: 1rem; font-size: 0.9rem; }
.error { background: #fee; border: 1px solid #c33; color: #900; padding: 0.6rem 0.8rem; border-radius: 4px; margin-bottom: 1rem; }
table.readonly { border-collapse: collapse; width: 100%; margin-bottom: 0.5rem; }
table.readonly td { padding: 0.25rem 0.5rem; border-bottom: 1px solid #8882; }
table.readonly td.k { color: #666; width: 18rem; }
table.readonly td.v { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.field { display: grid; grid-template-columns: 18rem 1fr; gap: 0.5rem 1rem; align-items: center; padding: 0.3rem 0; border-bottom: 1px solid #8882; }
.field label { color: #444; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.field .control input, .field .control select, .field .control textarea { width: 100%; max-width: 32rem; padding: 0.3rem 0.4rem; box-sizing: border-box; }
.field .control input[readonly], .field .control textarea[readonly] { background: #f4f4f4; color: #333; }
.field .help { grid-column: 2; color: #666; font-size: 0.85rem; }
.field .help .meta { color: #888; font-size: 0.8rem; }
.actions { margin-top: 1rem; display: flex; gap: 0.6rem; }
.actions a, .actions button { padding: 0.45rem 0.9rem; font-size: 0.95rem; cursor: pointer; }
.actions a { text-decoration: none; border: 1px solid #888; border-radius: 4px; color: inherit; background: transparent; }
.actions button[disabled] { opacity: 0.5; cursor: not-allowed; }
.api-hint { margin-top: 1.5rem; color: #888; font-size: 0.85rem; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
@media (prefers-color-scheme: dark) {
  .field .control input[readonly], .field .control textarea[readonly] { background: #222; color: #ccc; }
  .error { background: #4a1a1a; color: #fdd; border-color: #c33; }
}
</style>
</head>
<body>
<h1>cllm runtime configuration</h1>
<p class="subtitle">{{if .Edit}}Editing &mdash; modify any field, then save. Validation runs server-side; HTML5 hints are provided client-side.{{else}}Read-only view. Click <em>Edit</em> to change values.{{end}}</p>

{{if .Error}}<div class="error"><strong>Validation error:</strong> {{.Error}}</div>{{end}}

<h2>Status (read-only)</h2>
<table class="readonly">
{{range .ReadOnly}}<tr><td class="k">{{.Label}}</td><td class="v">{{.Current}}</td></tr>
{{end}}</table>

{{if not .Edit}}
<div class="actions">
  <a href="/config?edit=1">Edit</a>
</div>
{{end}}

{{if .Edit}}<form method="POST" action="/config" id="cfgform">{{end}}

{{range .Sections}}
<h2>{{.Title}}</h2>
{{range .Fields}}
<div class="field">
  <label for="f-{{.Key}}">{{.Label}}</label>
  <div class="control">
    {{if eq .Kind 3}}{{/* fieldSelect */}}
      {{$cur := .Current}}
      <select id="f-{{.Key}}" name="{{.Key}}" {{if not $.Edit}}disabled{{end}} data-original="{{.Current}}">
        <option value=""{{if eq $cur ""}} selected{{end}}>(none)</option>
        {{range .Options}}<option value="{{.}}"{{if eq . $cur}} selected{{end}}>{{.}}</option>{{end}}
      </select>
    {{else if eq .Kind 4}}{{/* fieldPassword */}}
      <input type="password" id="f-{{.Key}}" name="{{.Key}}" placeholder="(unchanged)" value="" autocomplete="off" {{if not $.Edit}}readonly{{end}} data-original="">
    {{else if eq .Kind 0}}{{/* fieldInt */}}
      <input type="number" id="f-{{.Key}}" name="{{.Key}}" value="{{.Current}}" {{if .Min}}min="{{.Min}}"{{end}} {{if .Max}}max="{{.Max}}"{{end}} step="1" {{if not $.Edit}}readonly{{end}} data-original="{{.Current}}">
    {{else if eq .Kind 1}}{{/* fieldFloat */}}
      <input type="number" id="f-{{.Key}}" name="{{.Key}}" value="{{.Current}}" {{if .Min}}min="{{.Min}}"{{end}} {{if .Max}}max="{{.Max}}"{{end}} step="{{if .Step}}{{.Step}}{{else}}any{{end}}" {{if not $.Edit}}readonly{{end}} data-original="{{.Current}}">
    {{else}}{{/* fieldString */}}
      <input type="text" id="f-{{.Key}}" name="{{.Key}}" value="{{.Current}}" {{if not $.Edit}}readonly{{end}} data-original="{{.Current}}">
    {{end}}
  </div>
  <div class="help">{{.Help}}<span class="meta">
    {{if .Default}} &nbsp;default: <code>{{.Default}}</code>{{end}}
    {{if and .Min .Max}} &nbsp;range: <code>{{.Min}}</code>&ndash;<code>{{.Max}}</code>{{else if .Min}} &nbsp;min: <code>{{.Min}}</code>{{else if .Max}} &nbsp;max: <code>{{.Max}}</code>{{end}}
  </span></div>
</div>
{{end}}
{{end}}

<div class="actions">
{{if .Edit}}
  <button type="submit" id="save-btn" disabled>Save</button>
  <a href="/config">Cancel</a>
{{end}}
</div>

{{if .Edit}}</form>
<script>
(function(){
  var form = document.getElementById('cfgform');
  if (!form) return;
  var save = document.getElementById('save-btn');
  function check() {
    var dirty = false;
    var inputs = form.querySelectorAll('input[data-original], select[data-original]');
    for (var i = 0; i < inputs.length; i++) {
      var el = inputs[i];
      if (el.type === 'password') {
        if (el.value !== '') { dirty = true; break; }
        continue;
      }
      if ((el.value || '') !== (el.getAttribute('data-original') || '')) { dirty = true; break; }
    }
    save.disabled = !dirty || !form.checkValidity();
  }
  form.addEventListener('input', check);
  form.addEventListener('change', check);
  check();
})();
</script>
{{end}}

<p class="api-hint">JSON API still works: <code>curl -H 'Accept: application/json' http://&hellip;/config</code> &middot; query-string updates: <code>GET /config?max_tokens=2000</code></p>

</body>
</html>
`
