package orchestrator

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

type dashboardWorkspace struct {
	UUID               string `json:"uuid"`
	Name               string `json:"name"`
	Generation         uint64 `json:"generation"`
	ObservedGeneration uint64 `json:"observed_generation"`
	Active             bool   `json:"active"`
	Ready              bool   `json:"ready"`
	SandboxUUID        string `json:"sandbox_uuid,omitempty"`
	DiskUsedBytes      uint64 `json:"disk_used_bytes,omitempty"`
	DiskAllowanceBytes uint64 `json:"disk_allowance_bytes,omitempty"`
}

type dashboardNode struct {
	Fingerprint string               `json:"fingerprint"`
	Hostname    string               `json:"hostname,omitempty"`
	Version     string               `json:"version,omitempty"`
	CPUs        uint32               `json:"cpus,omitempty"`
	MemoryBytes uint64               `json:"memory_bytes,omitempty"`
	LastSeenAt  time.Time            `json:"last_seen_at,omitempty"`
	Connected   bool                 `json:"connected"`
	Workspaces  []dashboardWorkspace `json:"workspaces"`
}

type dashboardStatus struct {
	Status         string               `json:"status"`
	GeneratedAt    time.Time            `json:"generated_at"`
	NodeCount      int                  `json:"node_count"`
	WorkspaceCount int                  `json:"workspace_count"`
	Nodes          []dashboardNode      `json:"nodes"`
	Unassigned     []dashboardWorkspace `json:"unassigned_workspaces"`
}

func dashboardHandler(nodes *nodectrl.Registry, workspaces *workspacecatalog.Store) http.Handler {
	mux := http.NewServeMux()
	page := newDashboardPage()
	status := func() dashboardStatus { return buildDashboardStatus(nodes, workspaces, time.Now().UTC()) }
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeDashboardJSON(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		writeDashboardJSON(w, status())
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := page.Execute(w, status()); err != nil {
			http.Error(w, "render dashboard", http.StatusInternalServerError)
		}
	})
	return mux
}

func buildDashboardStatus(nodes *nodectrl.Registry, workspaces *workspacecatalog.Store, now time.Time) dashboardStatus {
	entries := nodes.Snapshot()
	groups := make(map[string]int, len(entries))
	out := dashboardStatus{Status: "ok", GeneratedAt: now, Nodes: make([]dashboardNode, 0, len(entries)), Unassigned: []dashboardWorkspace{}}
	for i := range entries {
		entry := &entries[i]
		fingerprint := entry.Fingerprint.String()
		out.Nodes = append(out.Nodes, dashboardNode{
			Fingerprint: fingerprint, Hostname: entry.Hostname, Version: entry.Version,
			CPUs: entry.Resources.CPUs, MemoryBytes: entry.Resources.MemoryBytes,
			LastSeenAt: entry.LastSeenAt, Connected: true, Workspaces: []dashboardWorkspace{},
		})
		groups[fingerprint] = len(out.Nodes) - 1
	}
	allWorkspaces := workspaces.List(nil)
	for i := range allWorkspaces {
		workspace := &allWorkspaces[i]
		view := dashboardWorkspace{
			UUID: workspace.Metadata.UUID, Name: workspace.Metadata.Name,
			Generation: workspace.Metadata.Generation, ObservedGeneration: workspace.Status.ObservedGeneration,
			Ready:         workspace.Status.IsTrue(workspacecatalog.ConditionReady),
			DiskUsedBytes: workspace.Status.DiskUsedBytes, DiskAllowanceBytes: workspace.Status.DiskAllowanceBytes,
		}
		target := workspace.Spec.TargetSandboxFingerprint
		if workspace.Status.CurrentSandbox != nil {
			view.Active = true
			view.SandboxUUID = workspace.Status.CurrentSandbox.UID
			target = workspace.Status.CurrentSandbox.NodeFingerprint.String()
		}
		if group, ok := groups[target]; ok {
			out.Nodes[group].Workspaces = append(out.Nodes[group].Workspaces, view)
		} else if target != "" {
			out.Nodes = append(out.Nodes, dashboardNode{Fingerprint: target, Connected: false, Workspaces: []dashboardWorkspace{view}})
			groups[target] = len(out.Nodes) - 1
		} else {
			out.Unassigned = append(out.Unassigned, view)
		}
	}
	sort.Slice(out.Nodes, func(i, j int) bool {
		if out.Nodes[i].Connected != out.Nodes[j].Connected {
			return out.Nodes[i].Connected
		}
		return out.Nodes[i].Fingerprint < out.Nodes[j].Fingerprint
	})
	for i := range out.Nodes {
		sort.Slice(out.Nodes[i].Workspaces, func(a, b int) bool { return out.Nodes[i].Workspaces[a].Name < out.Nodes[i].Workspaces[b].Name })
	}
	sort.Slice(out.Unassigned, func(i, j int) bool { return out.Unassigned[i].Name < out.Unassigned[j].Name })
	out.NodeCount = len(entries)
	out.WorkspaceCount = len(allWorkspaces)
	return out
}

func writeDashboardJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode dashboard response", http.StatusInternalServerError)
		return
	}
	data = append(data, '\n')
	_, _ = w.Write(data) // The client disconnect is not recoverable inside a completed HTTP handler.
}

func newDashboardPage() *template.Template {
	return template.Must(template.New("dashboard").Funcs(template.FuncMap{
		"stamp": func(value time.Time) string {
			if value.IsZero() {
				return "never"
			}
			return value.Local().Format("2006-01-02 15:04:05 MST")
		},
		"gib": formatGiB,
	}).Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>DarkAIFactory orchestrator</title><style>
:root{color-scheme:light dark;--bg:#0c1117;--panel:#151d27;--line:#2d3b4d;--text:#edf3f8;--muted:#9fb0c1;--accent:#60d394;--warn:#ffca6a}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:15px/1.5 ui-sans-serif,system-ui,sans-serif}main{max-width:1180px;margin:auto;padding:32px 20px 64px}h1{font-size:2.1rem;margin:0}h2{margin:34px 0 12px}.summary{display:flex;gap:14px;flex-wrap:wrap;margin:18px 0}.pill,.card{background:var(--panel);border:1px solid var(--line);border-radius:10px}.pill{padding:8px 12px}.card{padding:16px;margin:12px 0}.ok{color:var(--accent)}.warn{color:var(--warn)}.muted{color:var(--muted)}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:10px 12px;border-bottom:1px solid var(--line);vertical-align:top}th{color:var(--muted);font-weight:600}code{font:13px/1.45 ui-monospace,SFMono-Regular,Consolas,monospace}.state{display:inline-block;padding:2px 7px;border:1px solid var(--line);border-radius:999px;font-size:12px}.active{border-color:var(--accent);color:var(--accent)}@media(max-width:760px){table{display:block;overflow-x:auto}}
</style></head><body><main><h1>DarkAIFactory</h1><div class="muted">Orchestrator fleet and workspace status</div><div class="summary"><div class="pill"><span class="ok">● healthy</span></div><div class="pill">{{.NodeCount}} connected nodes</div><div class="pill">{{.WorkspaceCount}} workspaces</div><div class="pill">updated {{stamp .GeneratedAt}}</div></div>
<h2>Nodes</h2>{{if .Nodes}}{{range .Nodes}}<section class="card"><h3>{{if .Hostname}}{{.Hostname}}{{else}}Unnamed node{{end}} {{if .Connected}}<span class="ok">● connected</span>{{else}}<span class="warn">● disconnected</span>{{end}}</h3><div class="muted"><code>{{.Fingerprint}}</code>{{if .Version}} · version {{.Version}}{{end}}{{if .Connected}} · {{.CPUs}} CPUs · {{gib .MemoryBytes}} GiB RAM · seen {{stamp .LastSeenAt}}{{end}}</div>{{if .Workspaces}}<table><thead><tr><th>Workspace</th><th>State</th><th>Generation</th><th>Storage</th><th>Identity</th></tr></thead><tbody>{{range .Workspaces}}<tr><td>{{.Name}}</td><td>{{if .Active}}<span class="state active">active</span>{{else}}<span class="state">inactive</span>{{end}} {{if .Ready}}ready{{else}}not ready{{end}}</td><td>{{.ObservedGeneration}} / {{.Generation}}</td><td>{{gib .DiskUsedBytes}} / {{gib .DiskAllowanceBytes}} GiB</td><td><code>{{.UUID}}</code>{{if .SandboxUUID}}<br><small>Sandbox <code>{{.SandboxUUID}}</code></small>{{end}}</td></tr>{{end}}</tbody></table>{{else}}<p class="muted">No workspaces target this node.</p>{{end}}</section>{{end}}{{else}}<div class="card muted">No nodes are connected.</div>{{end}}
{{if .Unassigned}}<h2>Unassigned workspaces</h2><section class="card"><table><thead><tr><th>Workspace</th><th>State</th><th>Generation</th><th>Identity</th></tr></thead><tbody>{{range .Unassigned}}<tr><td>{{.Name}}</td><td>inactive</td><td>{{.ObservedGeneration}} / {{.Generation}}</td><td><code>{{.UUID}}</code></td></tr>{{end}}</tbody></table></section>{{end}}<p class="muted"><a href="/v1/status">JSON status</a> · <a href="/healthz">health</a></p></main></body></html>`))
}

func formatGiB(value uint64) string {
	const gib = uint64(1024 * 1024 * 1024)
	return fmt.Sprintf("%.1f", float64(value)/float64(gib))
}
