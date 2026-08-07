package orchestrator

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/richiejp/darkaifactory/internal/agentruncatalog"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

const dashboardEventKeepalive = 15 * time.Second

const dashboardLiveJS = `(function(){
var source=new EventSource('/events'),timer=null;
source.addEventListener('change',function(){clearTimeout(timer);timer=setTimeout(function(){location.reload()},300)});
})();`

type dashboardCondition struct {
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	Message   string    `json:"message,omitempty"`
	ChangedAt time.Time `json:"changed_at,omitempty"`
}

type dashboardRun struct {
	UUID        string    `json:"uuid"`
	Profile     string    `json:"profile"`
	State       string    `json:"state"`
	SandboxUUID string    `json:"sandbox_uuid,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type dashboardWorkspaceDetail struct {
	GeneratedAt        time.Time                      `json:"generated_at"`
	Workspace          dashboardWorkspace             `json:"workspace"`
	TargetNode         string                         `json:"target_node,omitempty"`
	ImageRef           string                         `json:"image_ref,omitempty"`
	DevcontainerPath   string                         `json:"devcontainer_path,omitempty"`
	CPU                uint32                         `json:"cpu_cores,omitempty"`
	MemoryBytes        uint64                         `json:"memory_bytes,omitempty"`
	RequestedDiskBytes uint64                         `json:"requested_disk_bytes,omitempty"`
	ProtectedBytes     uint64                         `json:"protected_bytes,omitempty"`
	ManifestDigest     string                         `json:"manifest_digest,omitempty"`
	ManifestGeneration uint64                         `json:"manifest_generation,omitempty"`
	Frozen             bool                           `json:"storage_frozen"`
	Missing            []string                       `json:"missing,omitempty"`
	EnvironmentNames   []string                       `json:"environment_names,omitempty"`
	Mounts             []workspacecatalog.Mount       `json:"mounts,omitempty"`
	Ports              []workspacecatalog.PortMapping `json:"ports,omitempty"`
	Conditions         []dashboardCondition           `json:"conditions"`
	Runs               []dashboardRun                 `json:"agent_runs"`
}

type dashboardNodeDetail struct {
	GeneratedAt time.Time     `json:"generated_at"`
	Node        dashboardNode `json:"node"`
}

func registerDashboardDetailRoutes(mux *http.ServeMux, nodes *nodectrl.Registry, workspaces *workspacecatalog.Store, runs *agentruncatalog.Store) {
	workspacePage := newDashboardWorkspacePage()
	nodePage := newDashboardNodePage()
	mux.HandleFunc("GET /workspaces/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		detail, err := buildDashboardWorkspaceDetail(workspaces, runs, r.PathValue("uuid"), time.Now().UTC())
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeDashboardPage(w, workspacePage, detail)
	})
	mux.HandleFunc("GET /v1/workspaces/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		detail, err := buildDashboardWorkspaceDetail(workspaces, runs, r.PathValue("uuid"), time.Now().UTC())
		if err != nil {
			http.NotFound(w, r)
			return
		}
		writeDashboardJSON(w, detail)
	})
	mux.HandleFunc("GET /sandboxes/{uid}", func(w http.ResponseWriter, r *http.Request) {
		workspace, ok := workspaceForSandbox(workspaces, r.PathValue("uid"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/workspaces/"+workspace.Metadata.UUID+"#sandbox", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /nodes/{fingerprint}", func(w http.ResponseWriter, r *http.Request) {
		detail, ok := buildDashboardNodeDetail(nodes, workspaces, r.PathValue("fingerprint"), time.Now().UTC())
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeDashboardPage(w, nodePage, detail)
	})
}

func writeDashboardPage(w http.ResponseWriter, page *template.Template, value any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := page.Execute(w, value); err != nil {
		http.Error(w, "render dashboard", http.StatusInternalServerError)
	}
}

func workspaceForSandbox(workspaces *workspacecatalog.Store, uid string) (workspacecatalog.Workspace, bool) {
	list := workspaces.List(func(workspace *workspacecatalog.Workspace) bool {
		return workspace.Status.CurrentSandbox != nil && workspace.Status.CurrentSandbox.UID == uid
	})
	if len(list) != 1 {
		return workspacecatalog.Workspace{}, false
	}
	return list[0], true
}

func buildDashboardWorkspaceDetail(workspaces *workspacecatalog.Store, runs *agentruncatalog.Store, uuid string, now time.Time) (dashboardWorkspaceDetail, error) {
	workspace, err := workspaces.Get(uuid)
	if err != nil {
		return dashboardWorkspaceDetail{}, err
	}
	view := dashboardWorkspace{
		UUID: uuid, Name: workspace.Metadata.Name, Generation: workspace.Metadata.Generation,
		ObservedGeneration: workspace.Status.ObservedGeneration, Ready: workspace.Status.IsTrue(workspacecatalog.ConditionReady),
		DiskUsedBytes: workspace.Status.DiskUsedBytes, DiskAllowanceBytes: workspace.Status.DiskAllowanceBytes,
	}
	if workspace.Status.CurrentSandbox != nil {
		view.Active = true
		view.SandboxUUID = workspace.Status.CurrentSandbox.UID
	}
	detail := dashboardWorkspaceDetail{
		GeneratedAt: now, Workspace: view, TargetNode: workspace.Spec.TargetSandboxFingerprint,
		ImageRef: workspace.Spec.ImageRef, DevcontainerPath: workspace.Spec.DevcontainerPath, CPU: workspace.Spec.CPUCores,
		MemoryBytes: workspace.Spec.MemoryBytes, RequestedDiskBytes: workspace.Spec.DiskBytes, ProtectedBytes: workspace.Status.ProtectedBytes,
		ManifestDigest: workspace.Status.ManifestDigest, ManifestGeneration: workspace.Status.ManifestGeneration,
		Frozen: workspace.Status.IsTrue(workspacecatalog.ConditionStorageFrozen), Missing: workspace.Status.Missing,
		Mounts: workspace.Spec.Mounts, Ports: workspace.Spec.Ports,
	}
	for _, env := range workspace.Spec.Env {
		detail.EnvironmentNames = append(detail.EnvironmentNames, env.Name)
	}
	for _, condition := range workspace.Status.Conditions {
		detail.Conditions = append(detail.Conditions, dashboardCondition{Type: condition.Type, Status: condition.Status.String(), Reason: condition.Reason, Message: condition.Message, ChangedAt: condition.LastTransitionTime})
	}
	if runs != nil {
		workspaceRuns := runs.List(workspace.Metadata.OwnerFingerprint)
		for i := range workspaceRuns {
			run := &workspaceRuns[i]
			if run.WorkspaceUUID == uuid {
				detail.Runs = append(detail.Runs, dashboardRun{UUID: run.UUID, Profile: run.ProfileName, State: string(run.State), SandboxUUID: run.SandboxUID, Reason: run.Reason, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt})
			}
		}
		sort.Slice(detail.Runs, func(i, j int) bool { return detail.Runs[i].UpdatedAt.After(detail.Runs[j].UpdatedAt) })
	}
	return detail, nil
}

func buildDashboardNodeDetail(nodes *nodectrl.Registry, workspaces *workspacecatalog.Store, fingerprint string, now time.Time) (dashboardNodeDetail, bool) {
	status := buildDashboardStatus(nodes, workspaces, now)
	for _, node := range status.Nodes {
		if node.Fingerprint == fingerprint {
			return dashboardNodeDetail{GeneratedAt: now, Node: node}, true
		}
	}
	return dashboardNodeDetail{}, false
}

func serveDashboardEvents(w http.ResponseWriter, r *http.Request, nodes *nodectrl.Registry, workspaces *workspacecatalog.Store, runs *agentruncatalog.Store) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	nodeChanges, stopNodes := nodes.Watch()
	workspaceChanges, stopWorkspaces := workspaces.Watch()
	defer stopNodes()
	defer stopWorkspaces()
	var runChanges <-chan uint64
	stopRuns := func() {}
	if runs != nil {
		runChanges, stopRuns = runs.Watch()
	}
	defer stopRuns()
	if _, err := fmt.Fprint(w, "retry: 2000\nevent: ready\ndata: ready\n\n"); err != nil {
		return
	}
	flusher.Flush()
	keepalive := time.NewTicker(dashboardEventKeepalive)
	defer keepalive.Stop()
	//daft:lint:disable=forloopbound reason="the request context or client write failure terminates this long-lived SSE stream"
	for {
		select {
		case <-r.Context().Done():
			return
		case <-nodeChanges:
		case <-workspaceChanges:
		case <-runChanges:
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		}
		if _, err := fmt.Fprint(w, "event: change\ndata: changed\n\n"); err != nil {
			return
		}
		flusher.Flush()
	}
}

func newDashboardWorkspacePage() *template.Template {
	return template.Must(template.New("workspace").Funcs(dashboardTemplateFuncs()).Parse(detailPageStart + `<p><a href="/">← dashboard</a></p><h1>{{.Workspace.Name}}</h1><div class="muted"><code>{{.Workspace.UUID}}</code> · updated {{stamp .GeneratedAt}}</div>
<div class="grid"><section class="card"><h2>State</h2><p>{{if .Workspace.Active}}<span class="ok">● active</span>{{else}}inactive{{end}} · {{if .Workspace.Ready}}ready{{else}}not ready{{end}}{{if .Frozen}} · <span class="warn">storage frozen</span>{{end}}</p><p>Generation {{.Workspace.ObservedGeneration}} / {{.Workspace.Generation}}</p>{{if .Missing}}<p class="muted">Missing: {{join .Missing}}</p>{{end}}</section>
<section class="card" id="sandbox"><h2>Sandbox</h2>{{if .Workspace.SandboxUUID}}<p><code>{{.Workspace.SandboxUUID}}</code></p>{{else}}<p class="muted">No current Sandbox.</p>{{end}}<p>Node <code>{{.TargetNode}}</code></p></section>
<section class="card"><h2>Storage and CAS</h2><p>{{gib .Workspace.DiskUsedBytes}} / {{gib .Workspace.DiskAllowanceBytes}} GiB retained disk</p><p>{{gib .ProtectedBytes}} GiB protected · requested {{gib .RequestedDiskBytes}} GiB</p><p>Manifest generation {{.ManifestGeneration}}</p><code>{{.ManifestDigest}}</code></section></div>
<section class="card"><h2>Declaration</h2><p>Image <code>{{.ImageRef}}</code>{{if .DevcontainerPath}} · devcontainer <code>{{.DevcontainerPath}}</code>{{end}}</p><p>{{.CPU}} CPUs · {{gib .MemoryBytes}} GiB memory</p><p>Environment names: {{if .EnvironmentNames}}{{join .EnvironmentNames}}{{else}}none{{end}}</p><p>{{len .Mounts}} mounts · {{len .Ports}} forwarded ports</p></section>
<section class="card"><h2>Conditions</h2>{{if .Conditions}}<table><tr><th>Type</th><th>Status</th><th>Reason</th><th>Changed</th></tr>{{range .Conditions}}<tr><td>{{.Type}}</td><td>{{.Status}}</td><td>{{.Reason}}<br><span class="muted">{{.Message}}</span></td><td>{{stamp .ChangedAt}}</td></tr>{{end}}</table>{{else}}<p class="muted">No conditions.</p>{{end}}</section>
<section class="card"><h2>Agent runs</h2>{{if .Runs}}<table><tr><th>Run</th><th>Profile</th><th>State</th><th>Updated</th></tr>{{range .Runs}}<tr><td><code>{{.UUID}}</code></td><td>{{.Profile}}</td><td>{{.State}}{{if .Reason}}<br><span class="muted">{{.Reason}}</span>{{end}}</td><td>{{stamp .UpdatedAt}}</td></tr>{{end}}</table>{{else}}<p class="muted">No AgentRuns for this Workspace.</p>{{end}}</section>` + detailPageEnd))
}

func newDashboardNodePage() *template.Template {
	return template.Must(template.New("node").Funcs(dashboardTemplateFuncs()).Parse(detailPageStart + `<p><a href="/">← dashboard</a></p><h1>{{if .Node.Hostname}}{{.Node.Hostname}}{{else}}Unnamed node{{end}}</h1><p><code>{{.Node.Fingerprint}}</code></p><div class="card"><p>{{if .Node.Connected}}<span class="ok">● connected</span>{{else}}<span class="warn">● disconnected</span>{{end}} · {{.Node.Version}} · {{.Node.CPUs}} CPUs · {{gib .Node.MemoryBytes}} GiB RAM</p><p class="muted">Last seen {{stamp .Node.LastSeenAt}}</p></div><section class="card"><h2>Workspaces</h2>{{if .Node.Workspaces}}<table><tr><th>Name</th><th>State</th><th>Storage</th></tr>{{range .Node.Workspaces}}<tr><td><a href="/workspaces/{{.UUID}}">{{.Name}}</a></td><td>{{if .Active}}active{{else}}inactive{{end}}</td><td>{{gib .DiskUsedBytes}} / {{gib .DiskAllowanceBytes}} GiB</td></tr>{{end}}</table>{{else}}<p class="muted">No Workspaces target this Node.</p>{{end}}</section>` + detailPageEnd))
}

const (
	detailPageStart = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>darkaifactory</title><style>:root{color-scheme:dark;--bg:#0c1117;--panel:#151d27;--line:#2d3b4d;--text:#edf3f8;--muted:#9fb0c1;--accent:#60d394;--warn:#ffca6a}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,sans-serif}main{max-width:1180px;margin:auto;padding:28px 20px 64px}a{color:var(--accent)}.muted{color:var(--muted)}.ok{color:var(--accent)}.warn{color:var(--warn)}.card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:16px;margin:12px 0}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:12px}.grid .card{margin:12px 0}h2{margin-top:0}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:9px;border-bottom:1px solid var(--line);vertical-align:top}code{overflow-wrap:anywhere}</style></head><body><main>`
	detailPageEnd   = `</main><script>` + dashboardLiveJS + `</script></body></html>`
)
