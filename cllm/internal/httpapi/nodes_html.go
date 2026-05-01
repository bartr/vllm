package httpapi

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// nodesTemplateData is the root struct passed to the /nodes list template.
type nodesTemplateData struct {
	RouterPolicy string
	Count        int
	Nodes        []nodesRow
	CanDelete    bool // false when there is exactly one node (last-node rule)
}

// nodesRow is a flattened, presentation-friendly view of a single node.
// Live-stat counters and capacity fields are pre-formatted so the template
// can stay declarative.
type nodesRow struct {
	ID    string
	Class string

	UpstreamURL   string
	UpstreamModel string
	Synthetic     bool // true when no upstream is configured

	// Capacity (formatted; "0" rendered as "—" for axes that are disabled)
	MaxTokensInFlight  string
	MaxWaitingRequests string
	MaxConcurrency     string
	MaxKVTokens        string
	PerRequestTPS      string
	BypassCache        bool

	// Live runtime stats (always rendered, even when zero)
	TokensInFlight     string
	WaitingRequests    string
	KVTokensInFlight   string
	ConcurrentRequests string
	ConcurrencyWaiters string
}

// nodeFormState carries everything the edit/create template needs to render.
type nodeFormState struct {
	ID      string       // path-param id (empty when NewNode)
	NewNode bool         // true => /nodes?new=1 (empty form, ID input editable)
	Node    nodeResponse // live snapshot when editing
	Values  url.Values   // re-displayed POST values after a validation error
	Error   string
	Status  int
}

// nodeFormTemplateData is the root struct passed to the edit-form template.
type nodeFormTemplateData struct {
	NewNode  bool
	ID       string
	Action   string // form action URL (existing: /nodes/{id}; new: empty, JS sets it)
	Error    string
	IDField  nodeFormField // only rendered when NewNode
	Sections []nodeFormSection
}

type nodeFormSection struct {
	Title  string
	Fields []nodeFormField
}

type nodeFormField struct {
	Key     string // form name (snake_case)
	Label   string
	Kind    configFieldKind
	Help    string
	Min     string
	Max     string
	Step    string
	Current string   // pre-fill value (live or echoed POST)
	Options []string // for fieldSelect (e.g. ["true","false"])
}

func (h *Handler) renderNodesHTML(w http.ResponseWriter, _ *http.Request) {
	resp := h.currentNodes()

	rows := make([]nodesRow, 0, len(resp.Nodes))
	for _, n := range resp.Nodes {
		row := nodesRow{
			ID:                 n.ID,
			Class:              n.Class,
			MaxTokensInFlight:  fmtCapInt64(n.Capacity.MaxTokensInFlight),
			MaxWaitingRequests: fmtCapInt(n.Capacity.MaxWaitingRequests),
			MaxConcurrency:     fmtCapInt(n.Capacity.MaxConcurrency),
			MaxKVTokens:        fmtCapInt64(n.Capacity.MaxKVTokens),
			PerRequestTPS:      fmtCapInt(n.Capacity.PerRequestTPS),
			BypassCache:        n.Capacity.BypassCache,
			TokensInFlight:     strconv.FormatInt(n.Stats.TokensInFlight, 10),
			WaitingRequests:    strconv.Itoa(n.Stats.WaitingRequests),
			KVTokensInFlight:   strconv.FormatInt(n.Stats.KVTokensInFlight, 10),
			ConcurrentRequests: strconv.Itoa(n.Stats.ConcurrentRequests),
			ConcurrencyWaiters: strconv.Itoa(n.Stats.ConcurrencyWaiters),
		}
		if n.Upstream != nil {
			row.UpstreamURL = n.Upstream.URL
			row.UpstreamModel = n.Upstream.Model
		} else {
			row.Synthetic = true
		}
		rows = append(rows, row)
	}

	data := nodesTemplateData{
		RouterPolicy: resp.RouterPolicy,
		Count:        resp.Count,
		Nodes:        rows,
		CanDelete:    resp.Count > 1,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := nodesTmpl.Execute(w, data); err != nil {
		_, _ = fmt.Fprintf(w, "\n<!-- template error: %s -->\n", template.HTMLEscapeString(err.Error()))
	}
}

func (h *Handler) renderNodeEditHTML(w http.ResponseWriter, _ *http.Request, state nodeFormState) {
	// Build a lookup that prefers re-displayed POST values over the live
	// snapshot, so a validation error does not lose the user's edits.
	pickStr := func(key, current string) string {
		if state.Values != nil {
			if v := state.Values.Get(key); v != "" {
				return v
			}
		}
		return current
	}
	pickInt := func(key string, current int) string {
		if state.Values != nil {
			if v := state.Values.Get(key); v != "" {
				return v
			}
		}
		return strconv.Itoa(current)
	}
	pickInt64 := func(key string, current int64) string {
		if state.Values != nil {
			if v := state.Values.Get(key); v != "" {
				return v
			}
		}
		return strconv.FormatInt(current, 10)
	}
	pickFloat := func(key string, current float64) string {
		if state.Values != nil {
			if v := state.Values.Get(key); v != "" {
				return v
			}
		}
		return strconv.FormatFloat(current, 'f', -1, 64)
	}
	pickBool := func(key string, current bool) string {
		if state.Values != nil {
			if v := state.Values.Get(key); v != "" {
				return v
			}
		}
		if current {
			return "true"
		}
		return "false"
	}

	cap := state.Node.Capacity
	deg := state.Node.Degradation
	rl := state.Node.Realism
	upstreamURL, upstreamModel := "", ""
	if state.Node.Upstream != nil {
		upstreamURL = state.Node.Upstream.URL
		upstreamModel = state.Node.Upstream.Model
	}

	idField := nodeFormField{
		Key: "id", Label: "id", Kind: fieldString,
		Help:    "Unique node ID. Cannot contain whitespace or '/'.",
		Current: pickStr("id", ""),
	}

	sections := []nodeFormSection{
		{
			Title: "Identity",
			Fields: []nodeFormField{
				{
					Key: "class", Label: "class", Kind: fieldString,
					Help:    "Node class (matches a class in classes.yaml).",
					Current: pickStr("class", state.Node.Class),
				},
			},
		},
		{
			Title: "Capacity",
			Fields: []nodeFormField{
				{
					Key: "max_tokens_in_flight", Label: "max_tokens_in_flight", Kind: fieldInt,
					Help: "Per-node token-cost ceiling. 0 disables the cost gate.", Min: "0",
					Current: pickInt64("max_tokens_in_flight", cap.MaxTokensInFlight),
				},
				{
					Key: "max_waiting_requests", Label: "max_waiting_requests", Kind: fieldInt,
					Help: "Queue depth past the cost gate before 429.", Min: "0",
					Current: pickInt("max_waiting_requests", cap.MaxWaitingRequests),
				},
				{
					Key: "max_concurrency", Label: "max_concurrency", Kind: fieldInt,
					Help: "Concurrent request slots. 0 disables the slot gate.", Min: "0",
					Current: pickInt("max_concurrency", cap.MaxConcurrency),
				},
				{
					Key: "per_request_tokens_per_second", Label: "per_request_tokens_per_second", Kind: fieldInt,
					Help: "Per-request decode rate (tok/s). 0 disables per-request pacing.", Min: "0",
					Current: pickInt("per_request_tokens_per_second", cap.PerRequestTPS),
				},
				{
					Key: "degradation_threshold", Label: "degradation_threshold", Kind: fieldInt,
					Help: "Concurrent-request count where rate degradation begins. 0 disables the soft band.", Min: "0",
					Current: pickInt("degradation_threshold", cap.DegradationThreshold),
				},
				{
					Key: "max_degradation", Label: "max_degradation", Kind: fieldInt,
					Help: "Max f(load) degradation percent at MaxConcurrency. Range 0-95.", Min: "0", Max: "95",
					Current: pickInt("max_degradation", deg.MaxDegradation),
				},
				{
					Key: "bypass_cache", Label: "bypass_cache", Kind: fieldSelect,
					Help:    "Skip the response cache for every request routed to this node.",
					Options: []string{"false", "true"},
					Current: pickBool("bypass_cache", cap.BypassCache),
				},
			},
		},
		{
			Title: "KV Cache",
			Fields: []nodeFormField{
				{
					Key: "max_kv_tokens", Label: "max_kv_tokens", Kind: fieldInt,
					Help: "Per-node KV occupancy ceiling. 0 disables the KV axis entirely.", Min: "0",
					Current: pickInt64("max_kv_tokens", cap.MaxKVTokens),
				},
				{
					Key: "kv_weight", Label: "kv_weight", Kind: fieldFloat,
					Help: "Scales kv_load in combined-load routing. 0 falls back to 1.0.", Min: "0", Step: "0.1",
					Current: pickFloat("kv_weight", cap.KVWeight),
				},
				{
					Key: "kv_completion_factor", Label: "kv_completion_factor", Kind: fieldFloat,
					Help: "Scales the KV estimator's p95 completion prediction. 0 falls back to 1.0.", Min: "0", Max: "4", Step: "0.1",
					Current: pickFloat("kv_completion_factor", cap.KVCompletionFactor),
				},
			},
		},
		{
			Title: "Prefill Realism",
			Fields: []nodeFormField{
				{
					Key: "prefill_rate_multiplier", Label: "prefill_rate_multiplier", Kind: fieldFloat,
					Help: "Multiplier on prompt-token rate during prefill simulation. 0 disables.", Min: "0", Step: "0.1",
					Current: pickFloat("prefill_rate_multiplier", rl.PrefillRateMultiplier),
				},
				{
					Key: "prefill_base_overhead_ms", Label: "prefill_base_overhead_ms", Kind: fieldInt,
					Help: "Fixed base overhead per request (ms) before token streaming.", Min: "0",
					Current: pickInt("prefill_base_overhead_ms", rl.PrefillBaseOverheadMs),
				},
				{
					Key: "prefill_jitter_percent", Label: "prefill_jitter_percent", Kind: fieldInt,
					Help: "Random jitter (%) applied to prefill duration.", Min: "0", Max: "100",
					Current: pickInt("prefill_jitter_percent", rl.PrefillJitterPercent),
				},
				{
					Key: "prefill_max_ms", Label: "prefill_max_ms", Kind: fieldInt,
					Help: "Upper cap on simulated prefill duration (ms). 0 disables the cap.", Min: "0",
					Current: pickInt("prefill_max_ms", rl.PrefillMaxMs),
				},
			},
		},
		{
			Title: "Stream Realism",
			Fields: []nodeFormField{
				{
					Key: "stream_variability_percent", Label: "stream_variability_percent", Kind: fieldInt,
					Help: "Slow-rate variability across the stream (%).", Min: "0", Max: "100",
					Current: pickInt("stream_variability_percent", rl.StreamVariabilityPct),
				},
				{
					Key: "stream_jitter_percent", Label: "stream_jitter_percent", Kind: fieldInt,
					Help: "Per-token jitter (%) on stream pacing.", Min: "0", Max: "100",
					Current: pickInt("stream_jitter_percent", rl.StreamJitterPct),
				},
				{
					Key: "stream_stall_probability_percent", Label: "stream_stall_probability_percent", Kind: fieldInt,
					Help: "Probability per token of injecting a stall (%).", Min: "0", Max: "100",
					Current: pickInt("stream_stall_probability_percent", rl.StreamStallProbPct),
				},
				{
					Key: "stream_stall_min_ms", Label: "stream_stall_min_ms", Kind: fieldInt,
					Help: "Minimum stall duration (ms).", Min: "0",
					Current: pickInt("stream_stall_min_ms", rl.StreamStallMinMs),
				},
				{
					Key: "stream_stall_max_ms", Label: "stream_stall_max_ms", Kind: fieldInt,
					Help: "Maximum stall duration (ms).", Min: "0",
					Current: pickInt("stream_stall_max_ms", rl.StreamStallMaxMs),
				},
			},
		},
		{
			Title: "Upstream (passthrough)",
			Fields: []nodeFormField{
				{
					Key: "upstream", Label: "upstream", Kind: fieldString,
					Help:    "Upstream Chat Completions base URL. Leave blank for a synthetic node.",
					Current: pickStr("upstream", upstreamURL),
				},
				{
					Key: "model", Label: "model", Kind: fieldString,
					Help:    "Upstream model name (forwarded as-is in the JSON body).",
					Current: pickStr("model", upstreamModel),
				},
				{
					Key: "token", Label: "token", Kind: fieldPassword,
					Help:    "Bearer token for upstream calls. Leave blank to keep the current value.",
					Current: "",
				},
			},
		},
	}

	action := ""
	if !state.NewNode {
		action = "/nodes/" + state.ID
	}

	data := nodeFormTemplateData{
		NewNode:  state.NewNode,
		ID:       state.ID,
		Action:   action,
		Error:    state.Error,
		IDField:  idField,
		Sections: sections,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	status := state.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if err := nodeFormTmpl.Execute(w, data); err != nil {
		_, _ = fmt.Fprintf(w, "\n<!-- template error: %s -->\n", template.HTMLEscapeString(err.Error()))
	}
}

// fmtCapInt formats a capacity int, rendering 0 as an em-dash so disabled
// axes are visually distinct from genuinely-low values.
func fmtCapInt(v int) string {
	if v == 0 {
		return "—"
	}
	return strconv.Itoa(v)
}

func fmtCapInt64(v int64) string {
	if v == 0 {
		return "—"
	}
	return strconv.FormatInt(v, 10)
}

// nodeExists reports whether a node with the given id is currently
// registered. Used to decide whether a re-rendered edit form (after a
// validation error) is in "create" or "update" mode.
func nodeExists(h *Handler, id string) bool {
	if h == nil {
		return false
	}
	for _, existing := range h.NodeIDs() {
		if existing == id {
			return true
		}
	}
	return false
}

// formValues returns the parsed POST form values for the request, or nil
// on failure. Used to echo the user's submission back into the form when
// validation fails.
func formValues(r *http.Request) (url.Values, error) {
	if r == nil {
		return nil, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	out := url.Values{}
	for k, v := range r.PostForm {
		// Drop noise keys that shouldn't echo back.
		if strings.HasPrefix(k, "_") {
			continue
		}
		out[k] = v
	}
	return out, nil
}

var nodesTmpl = template.Must(template.New("nodes").Parse(nodesTemplateHTML))
var nodeFormTmpl = template.Must(template.New("nodeForm").Parse(nodeFormTemplateHTML))

const nodesCommonStyle = `
:root { color-scheme: light dark; }
body { font: 14px/1.45 system-ui, -apple-system, Segoe UI, sans-serif; margin: 1.5rem; max-width: 1200px; }
h1 { margin: 0 0 0.25rem; font-size: 1.4rem; }
h2 { margin: 1.25rem 0 0.4rem; font-size: 1.05rem; border-bottom: 1px solid #8884; padding-bottom: 0.2rem; }
.subtitle { color: #888; margin-bottom: 1rem; font-size: 0.9rem; }
.summary { color: #666; margin-bottom: 0.75rem; font-size: 0.9rem; }
.summary code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.actions { margin: 0.8rem 0; display: flex; gap: 0.6rem; flex-wrap: wrap; }
.actions a, .actions button { padding: 0.4rem 0.8rem; font-size: 0.9rem; cursor: pointer; text-decoration: none; border: 1px solid #888; border-radius: 4px; color: inherit; background: transparent; }
.actions button[disabled], .actions a[aria-disabled="true"] { opacity: 0.45; cursor: not-allowed; pointer-events: none; }
.error { background: #fee; border: 1px solid #c33; color: #900; padding: 0.6rem 0.8rem; border-radius: 4px; margin-bottom: 1rem; }
.api-hint { margin-top: 1.5rem; color: #888; font-size: 0.85rem; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
@media (prefers-color-scheme: dark) {
  .error { background: #4a1a1a; color: #fdd; border-color: #c33; }
}
`

const nodesTemplateHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>cllm /nodes</title>
<style>` + nodesCommonStyle + `
table.nodes { border-collapse: collapse; width: 100%; font-size: 0.88rem; }
table.nodes th, table.nodes td { padding: 0.35rem 0.6rem; border-bottom: 1px solid #8882; text-align: left; vertical-align: top; }
table.nodes th { background: #8881; font-weight: 600; }
table.nodes td.num { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; text-align: right; }
table.nodes td.id { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-weight: 600; }
table.nodes td.synthetic { color: #888; font-style: italic; }
table.nodes td.row-actions { white-space: nowrap; }
table.nodes td.row-actions a, table.nodes td.row-actions button { padding: 0.2rem 0.5rem; font-size: 0.8rem; margin-right: 0.25rem; cursor: pointer; text-decoration: none; border: 1px solid #888; border-radius: 3px; color: inherit; background: transparent; }
table.nodes td.row-actions button[disabled] { opacity: 0.45; cursor: not-allowed; }
.tag { display: inline-block; padding: 0.05rem 0.4rem; border-radius: 3px; font-size: 0.75rem; background: #8882; color: inherit; }
.empty { color: #888; font-style: italic; padding: 1rem 0; }
@media (prefers-color-scheme: dark) {
  table.nodes th { background: #2228; }
}
</style>
</head>
<body>
<h1>cllm nodes</h1>
<p class="subtitle">Read-only fleet view. Click <em>Edit</em> on any row to change values, or <em>Add</em> to register a new node.</p>

<p class="summary">
  router_policy: <code>{{if .RouterPolicy}}{{.RouterPolicy}}{{else}}(default){{end}}</code>
  &nbsp;&middot;&nbsp; nodes: <code>{{.Count}}</code>
</p>

<div class="actions">
  <a href="/nodes?new=1">+ Add node</a>
</div>

{{if .Nodes}}
<table class="nodes">
  <thead>
    <tr>
      <th>ID</th>
      <th>Class</th>
      <th>Upstream</th>
      <th class="num" title="max_tokens_in_flight">Max TIF</th>
      <th class="num" title="max_waiting_requests">Max Wait</th>
      <th class="num" title="max_concurrency">Max Conc</th>
      <th class="num" title="max_kv_tokens">Max KV</th>
      <th class="num" title="per_request_tokens_per_second">Per-req TPS</th>
      <th class="num" title="tokens_in_flight">TIF</th>
      <th class="num" title="waiting_requests">Wait</th>
      <th class="num" title="kv_tokens_in_flight">KV TIF</th>
      <th class="num" title="concurrent_requests">Conc</th>
      <th class="num" title="concurrency_waiting_requests">Conc Wait</th>
      <th>Actions</th>
    </tr>
  </thead>
  <tbody>
  {{range .Nodes}}
    <tr data-node-id="{{.ID}}">
      <td class="id">{{.ID}}{{if .BypassCache}} <span class="tag" title="bypass_cache">no-cache</span>{{end}}</td>
      <td>{{.Class}}</td>
      {{if .Synthetic}}<td class="synthetic">synthetic</td>{{else}}<td><code>{{.UpstreamURL}}</code>{{if .UpstreamModel}}<br><small>model: <code>{{.UpstreamModel}}</code></small>{{end}}</td>{{end}}
      <td class="num">{{.MaxTokensInFlight}}</td>
      <td class="num">{{.MaxWaitingRequests}}</td>
      <td class="num">{{.MaxConcurrency}}</td>
      <td class="num">{{.MaxKVTokens}}</td>
      <td class="num">{{.PerRequestTPS}}</td>
      <td class="num">{{.TokensInFlight}}</td>
      <td class="num">{{.WaitingRequests}}</td>
      <td class="num">{{.KVTokensInFlight}}</td>
      <td class="num">{{.ConcurrentRequests}}</td>
      <td class="num">{{.ConcurrencyWaiters}}</td>
      <td class="row-actions">
        <a href="/nodes/{{.ID}}?edit=1">Edit</a>
        <button type="button" class="del-btn" data-node-id="{{.ID}}" {{if not $.CanDelete}}disabled title="Cannot delete the last node"{{end}}>Delete</button>
      </td>
    </tr>
  {{end}}
  </tbody>
</table>
{{else}}
<p class="empty">No nodes registered.</p>
{{end}}

<p class="api-hint">JSON API: <code>curl -H 'Accept: application/json' /nodes</code> &middot; per-node detail: <code>/nodes/{id}</code></p>

<script>
(function(){
  document.querySelectorAll('button.del-btn').forEach(function(btn){
    btn.addEventListener('click', function(){
      var id = btn.getAttribute('data-node-id');
      if (!id) return;
      if (!confirm('Delete node "' + id + '"? This cannot be undone.')) return;
      btn.disabled = true;
      fetch('/nodes/' + encodeURIComponent(id), {method: 'DELETE'})
        .then(function(resp){
          if (resp.ok) { window.location.reload(); return; }
          return resp.text().then(function(t){ throw new Error(t || ('HTTP ' + resp.status)); });
        })
        .catch(function(err){
          alert('Delete failed: ' + err.message);
          btn.disabled = false;
        });
    });
  });
})();
</script>

</body>
</html>
`

const nodeFormTemplateHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>cllm {{if .NewNode}}/nodes (new){{else}}/nodes/{{.ID}}{{end}}</title>
<style>` + nodesCommonStyle + `
.field { display: grid; grid-template-columns: 18rem 1fr; gap: 0.5rem 1rem; align-items: center; padding: 0.3rem 0; border-bottom: 1px solid #8882; }
.field label { color: #444; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.field .control input, .field .control select, .field .control textarea { width: 100%; max-width: 32rem; padding: 0.3rem 0.4rem; box-sizing: border-box; }
.field .help { grid-column: 2; color: #666; font-size: 0.85rem; }
.field .help .meta { color: #888; font-size: 0.8rem; }
</style>
</head>
<body>
<h1>{{if .NewNode}}cllm: add node{{else}}cllm: edit node <code>{{.ID}}</code>{{end}}</h1>
<p class="subtitle">Per-request DSL &gt; class config &gt; node defaults. Empty <em>token</em> keeps the current upstream credential.</p>

{{if .Error}}<div class="error"><strong>Validation error:</strong> {{.Error}}</div>{{end}}

<form method="POST" {{if .Action}}action="{{.Action}}"{{end}} id="nodeform">

{{if .NewNode}}
<h2>Identity</h2>
<div class="field">
  <label for="f-id">{{.IDField.Label}}</label>
  <div class="control">
    <input type="text" id="f-id" name="id" value="{{.IDField.Current}}" required pattern="[^\s/]+" autofocus>
  </div>
  <div class="help">{{.IDField.Help}}</div>
</div>
{{end}}

{{range .Sections}}
<h2>{{.Title}}</h2>
{{range .Fields}}
<div class="field">
  <label for="f-{{.Key}}">{{.Label}}</label>
  <div class="control">
    {{if eq .Kind 4}}{{/* fieldPassword */}}
      <input type="password" id="f-{{.Key}}" name="{{.Key}}" placeholder="(unchanged)" value="" autocomplete="off">
    {{else if eq .Kind 3}}{{/* fieldSelect */}}
      {{$cur := .Current}}
      <select id="f-{{.Key}}" name="{{.Key}}">
        {{range .Options}}<option value="{{.}}"{{if eq . $cur}} selected{{end}}>{{.}}</option>{{end}}
      </select>
    {{else if eq .Kind 0}}{{/* fieldInt */}}
      <input type="number" id="f-{{.Key}}" name="{{.Key}}" value="{{.Current}}" {{if .Min}}min="{{.Min}}"{{end}} {{if .Max}}max="{{.Max}}"{{end}} step="1">
    {{else if eq .Kind 1}}{{/* fieldFloat */}}
      <input type="number" id="f-{{.Key}}" name="{{.Key}}" value="{{.Current}}" {{if .Min}}min="{{.Min}}"{{end}} {{if .Max}}max="{{.Max}}"{{end}} step="{{if .Step}}{{.Step}}{{else}}any{{end}}">
    {{else}}{{/* fieldString */}}
      <input type="text" id="f-{{.Key}}" name="{{.Key}}" value="{{.Current}}">
    {{end}}
  </div>
  <div class="help">{{.Help}}<span class="meta">
    {{if and .Min .Max}} &nbsp;range: <code>{{.Min}}</code>&ndash;<code>{{.Max}}</code>{{else if .Min}} &nbsp;min: <code>{{.Min}}</code>{{else if .Max}} &nbsp;max: <code>{{.Max}}</code>{{end}}
  </span></div>
</div>
{{end}}
{{end}}

<div class="actions">
  <button type="submit" id="save-btn">Save</button>
  <a href="/nodes">Cancel</a>
</div>

</form>

<script>
(function(){
  var form = document.getElementById('nodeform');
  if (!form) return;
  var isNew = {{if .NewNode}}true{{else}}false{{end}};
  if (isNew) {
    form.addEventListener('submit', function(e){
      var idEl = document.getElementById('f-id');
      var id = (idEl.value || '').trim();
      if (!id || /[\s/]/.test(id)) {
        e.preventDefault();
        alert('Invalid node ID: must be non-empty and contain no whitespace or "/".');
        return;
      }
      form.action = '/nodes/' + encodeURIComponent(id);
    });
  }
})();
</script>

</body>
</html>
`
