package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	protocol "github.com/evanxiao/quickworks/internal/protocol/agent"
	"github.com/evanxiao/quickworks/internal/server/agenthub"
	"github.com/evanxiao/quickworks/internal/server/auth"
	"github.com/evanxiao/quickworks/internal/server/workspace"
	controlweb "github.com/evanxiao/quickworks/web"
	"github.com/go-chi/chi/v5"
	"github.com/hashicorp/yamux"
	"gorm.io/gorm"
	"nhooyr.io/websocket"
)

type Router struct {
	auth                 *auth.Manager
	workspaces           *workspace.Service
	agents               *agenthub.Hub
	tunnels              *agenthub.Tunnels
	tcpTunnels           *agenthub.TCPHub
	db                   *gorm.DB
	provisionerTokenHash map[string]string
	workbenchConfig      AgentWorkbench
	templates            TemplateCatalog
	handler              http.Handler
	autoStopAfter        time.Duration
}

type AgentWorkbench struct {
	Version      string
	BundleURL    string
	BundleSHA256 string
	Entrypoint   string
	BYOKConfig   []byte
	Environment  []byte
}

type TemplateCatalog struct {
	Default        string
	Names          map[string]bool
	RequiredLabels map[string][]string
	Limits         map[string]workspace.Limits
	Resources      map[string]workspace.Resources
}

var labelPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func New(db *gorm.DB, a *auth.Manager, tokens map[string]string, release *ReleaseAssets, workbench AgentWorkbench, templates TemplateCatalog, autoStopAfter time.Duration) *Router {
	tokenHashes := make(map[string]string, len(tokens))
	for workerID, token := range tokens {
		tokenHashes[workerID] = hash(token)
	}
	r := &Router{auth: a, workspaces: workspace.NewWithPolicies(db, templates.Limits, templates.Resources), agents: agenthub.New(db), tunnels: agenthub.NewTunnels(), tcpTunnels: agenthub.NewTCPHub(), db: db, provisionerTokenHash: tokenHashes, workbenchConfig: workbench, templates: templates, autoStopAfter: autoStopAfter}
	router := chi.NewRouter()
	router.Use(requestID)
	router.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	router.Handle("/static/*", http.StripPrefix("/static/", controlweb.Static()))
	release.register(router)
	router.Get("/auth/github", a.Begin)
	router.Get("/auth/github/callback", a.Callback)
	router.Post("/api/agent/enroll", r.enroll)
	router.Post("/api/agent/ready", r.agentReady)
	router.Get("/api/agent/connect", r.agentConnect)
	router.Post("/api/internal/provisioner/leases", r.claim)
	router.Post("/api/internal/provisioner/leases/{id}/{action}", r.lease)
	router.Group(func(workbench chi.Router) {
		workbench.Use(a.RequireWorkbench)
		workbench.Handle("/w/{id}/*", http.HandlerFunc(r.workbench))
	})
	router.Group(func(pages chi.Router) {
		pages.Use(a.RequirePage)
		pages.Get("/", r.dashboard)
		pages.Get("/templates", r.templatesPage)
		pages.Get("/w/", r.createFromNavigation)
		pages.Get("/github/{owner}/{repo}", r.repository)
	})
	router.Group(func(api chi.Router) {
		api.Use(a.Require)
		api.Get("/api/me", r.me)
		api.Get("/api/workspaces", r.list)
		api.Get("/api/workspaces/{id}/readiness", r.readiness)
		api.Get("/api/quotas", r.quotas)
		api.Get("/api/provisioners", r.provisioners)
		api.Get("/api/templates", r.templateAvailability)
		api.Get("/api/scheduler-events", r.schedulerEvents)
		api.Post("/api/workspaces", r.create)
		api.Post("/api/workspaces/{id}/start", r.transition("start"))
		api.Post("/api/workspaces/{id}/stop", r.transition("stop"))
		api.Post("/api/workspaces/{id}/delete", r.transition("delete"))
		api.Delete("/api/workspaces/{id}", r.transition("delete"))
		api.Get("/api/workspaces/{id}/builds", r.builds)
		api.Get("/api/builds/{id}/logs", r.logs)
	})
	r.handler = router
	return r
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.handler.ServeHTTP(w, req)
}

func (r *Router) Reconcile(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		now := time.Now().UTC()
		_ = r.agents.MarkStale(ctx, now.Add(-45*time.Second))
		if r.autoStopAfter > 0 {
			_ = r.workspaces.StopIdle(ctx, now.Add(-r.autoStopAfter))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Router) createFromNavigation(w http.ResponseWriter, req *http.Request) {
	if !isTopLevelNavigation(req) {
		http.Error(w, "workspace creation requires a browser navigation", http.StatusForbidden)
		return
	}
	u, _ := auth.UserFromContext(req.Context())
	key := "navigation:" + r.auth.CSRF(req)
	templateName, err := r.template(req.URL.Query().Get("t"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ws, _, err := r.workspaces.CreateIdempotent(req.Context(), u.ID, key, "empty-navigation:"+templateName, "", templateName)
	if err != nil {
		http.Error(w, "create workspace", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, req, "/w/"+ws.ID+"/", http.StatusSeeOther)
}

func (r *Router) workbench(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	ws, err := r.workspaces.Get(req.Context(), u.ID, chi.URLParam(req, "id"))
	if err != nil {
		http.NotFound(w, req)
		return
	}
	if ws.ObservedState == "stopped" {
		r.workspaceStatusPage(w, req, ws, nil)
		return
	}
	if ws.ObservedState != "running" {
		builds, err := r.workspaces.Builds(req.Context(), u.ID, ws.ID)
		if err != nil {
			http.Error(w, "list workspace builds", http.StatusInternalServerError)
			return
		}
		var build *workspace.Build
		if len(builds) > 0 {
			build = &builds[0]
		}
		r.workspaceStatusPage(w, req, ws, build)
		return
	}
	if err := r.workspaces.Touch(req.Context(), u.ID, ws.ID); err != nil {
		http.Error(w, "update workspace activity", http.StatusInternalServerError)
		return
	}
	agentID, err := r.agents.ActiveAgent(req.Context(), ws.ID)
	if err != nil {
		builds, buildErr := r.workspaces.Builds(req.Context(), u.ID, ws.ID)
		if buildErr != nil {
			http.Error(w, "list workspace builds", http.StatusInternalServerError)
			return
		}
		var build *workspace.Build
		if len(builds) > 0 {
			build = &builds[0]
		}
		r.workspaceStatusPage(w, req, ws, build)
		return
	}
	target, _ := url.Parse("http://workbench")
	proxy := &httputil.ReverseProxy{}
	prefix := "/w/" + ws.ID
	proxy.Rewrite = func(proxyRequest *httputil.ProxyRequest) {
		path := strings.TrimPrefix(proxyRequest.In.URL.Path, prefix)
		if path == "" {
			path = "/"
		}
		proxyRequest.SetURL(target)
		proxyRequest.Out.URL.Path = path
		proxyRequest.Out.URL.RawPath = ""
		proxyRequest.Out.Header.Set("X-Forwarded-Prefix", prefix)
		proxyRequest.Out.Header.Set("X-Forwarded-Host", req.Host)
		proxyRequest.Out.Header.Set("X-Forwarded-Proto", requestScheme(req))
	}
	proxy.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return r.tcpTunnels.DialContext(ctx, agentID)
	}}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "workspace tunnel unavailable", http.StatusServiceUnavailable)
	}
	proxy.ServeHTTP(w, req)
}

func (r *Router) workspaceStatusPage(w http.ResponseWriter, req *http.Request, ws workspace.Workspace, build *workspace.Build) {
	const page = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Title}} · Quickworks</title>
  <link rel="stylesheet" href="/static/app.css">
  {{if .Build}}<link rel="stylesheet" href="/static/workspace-status.css">{{end}}
</head>
<body>
  <main class="mx-auto flex min-h-screen max-w-4xl items-center px-5 py-10 sm:px-8">
    <section class="w-full rounded-2xl bg-white p-6 shadow-sm ring-1 ring-slate-200 sm:p-8" data-workspace-status data-workspace-id="{{.Workspace.ID}}"{{if .Build}} data-build-id="{{.Build.ID}}"{{end}}>
      <h1 class="text-xl font-semibold tracking-tight text-ink">{{.Title}}</h1>
      <p id="workspace-status" class="mt-1 text-sm text-slate-500">{{.Message}}</p>
      {{if .Build}}
      <div class="mt-5 overflow-hidden rounded-xl bg-slate-950 p-3 shadow-inner"><div id="build-terminal" class="h-80 w-full" aria-label="Workspace build log"></div></div>
      {{end}}
      {{if .CanStart}}
      <form class="mt-6" method="post" action="/api/workspaces/{{.Workspace.ID}}/start">
        <input type="hidden" name="csrf_token" value="{{.CSRF}}">
        <input type="hidden" name="return_to" value="workbench">
        <button class="inline-flex items-center rounded-md bg-emerald-600 px-4 py-2.5 text-sm font-semibold text-white shadow-sm transition hover:bg-emerald-500 focus:outline-none focus:ring-2 focus:ring-emerald-600 focus:ring-offset-2" type="submit">Start workspace</button>
      </form>
      {{end}}
    </section>
  </main>
  {{if .Build}}<script type="module" src="/static/workspace-status.js"></script>{{end}}
</body>
</html>`

	view := map[string]any{
		"Workspace": ws,
		"Build":     build,
		"CSRF":      r.auth.CSRF(req),
		"Title":     "Preparing workspace",
		"Message":   "",
	}
	if ws.ObservedState == "stopped" {
		view["Title"] = "Workspace stopped"
		view["Message"] = "Start it to continue."
		view["CanStart"] = true
	} else if ws.ObservedState == "failed" {
		view["Title"] = "Build failed"
	} else if ws.ObservedState == "running" {
		view["Title"] = "Starting workspace"
	}
	template.Must(template.New("workspace-status").Parse(page)).Execute(w, view)
}

func (r *Router) workbenchWebSocket(w http.ResponseWriter, req *http.Request, workspaceID string) {
	agentID, err := r.agents.ActiveAgent(req.Context(), workspaceID)
	if err != nil {
		http.Error(w, "workspace agent is not ready", http.StatusServiceUnavailable)
		return
	}
	headers := safeHeaders(req.Header)
	headers.Set("X-Forwarded-Proto", requestScheme(req))
	headers.Set("X-Forwarded-Host", req.Host)
	headers.Set("X-Forwarded-Prefix", "/w/"+workspaceID)
	id := workspace.RandomID()
	tunnel, inbound, err := r.tunnels.OpenWebSocket(req.Context(), agentID, protocol.Frame{
		Type:    protocol.WebSocketOpen,
		ID:      id,
		Path:    req.URL.RequestURI(),
		Headers: headers,
	})
	if err != nil {
		http.Error(w, "workspace WebSocket upstream unavailable", http.StatusBadGateway)
		return
	}
	browser, err := websocket.Accept(w, req, nil)
	if err != nil {
		tunnel.CloseWebSocket(id, protocol.Frame{Type: protocol.WebSocketClose, ID: id, CloseCode: int(websocket.StatusGoingAway), CloseReason: "browser connection was rejected"})
		return
	}
	defer browser.Close(websocket.StatusNormalClosure, "workspace proxy closed")
	defer tunnel.CloseWebSocket(id, protocol.Frame{Type: protocol.WebSocketClose, ID: id, CloseCode: int(websocket.StatusNormalClosure), CloseReason: "browser connection closed"})
	proxyContext, cancel := context.WithCancel(req.Context())
	defer cancel()
	upstreamDone := make(chan struct{})
	go func() {
		defer close(upstreamDone)
		for frame := range inbound {
			switch frame.Type {
			case protocol.WebSocketMessage:
				messageType := websocket.MessageText
				if frame.MessageType == "binary" {
					messageType = websocket.MessageBinary
				}
				if err := browser.Write(proxyContext, messageType, frame.Body); err != nil {
					return
				}
			case protocol.WebSocketClose:
				code := websocket.StatusCode(frame.CloseCode)
				if code == 0 {
					code = websocket.StatusNormalClosure
				}
				_ = browser.Close(code, frame.CloseReason)
				return
			}
		}
	}()
	for {
		messageType, body, err := browser.Read(proxyContext)
		if err != nil {
			return
		}
		kind := "text"
		if messageType == websocket.MessageBinary {
			kind = "binary"
		}
		if err := tunnel.Send(protocol.Frame{Type: protocol.WebSocketMessage, ID: id, MessageType: kind, Body: body}); err != nil {
			return
		}
		select {
		case <-upstreamDone:
			return
		default:
		}
	}
}

func isTopLevelNavigation(req *http.Request) bool {
	acceptsHTML := strings.Contains(req.Header.Get("Accept"), "text/html")
	if !acceptsHTML || req.Header.Get("Sec-Fetch-Mode") != "navigate" || req.Header.Get("Sec-Fetch-Dest") != "document" {
		return false
	}
	site := req.Header.Get("Sec-Fetch-Site")
	if site != "none" && site != "same-origin" {
		return false
	}
	purpose := strings.ToLower(req.Header.Get("Purpose") + " " + req.Header.Get("Sec-Purpose"))
	return !strings.Contains(purpose, "prefetch")
}

func (r *Router) dashboard(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	ws, err := r.workspaces.List(req.Context(), u.ID)
	if err != nil {
		http.Error(w, "list workspaces", 500)
		return
	}
	const page = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Quickworks</title>
  <link rel="stylesheet" href="/static/app.css">
</head>
<body>
  <nav class="border-b border-slate-200 bg-white">
	    <div class="mx-auto flex h-16 w-full items-center justify-between px-5 sm:px-8 lg:px-12">
      <a class="text-lg font-semibold tracking-tight text-ink" href="/">Quickworks</a>
      <div class="flex items-center gap-6 text-sm"><a class="font-medium text-slate-600 transition hover:text-ink" href="#workspaces">Workspaces</a><a class="font-medium text-slate-600 transition hover:text-ink" href="/templates">Templates</a><span class="rounded-full bg-slate-100 px-3 py-1.5 font-medium text-slate-600">{{.User.GitHubLogin}}</span></div>
    </div>
  </nav>
  <main class="mx-auto min-h-screen max-w-6xl px-5 py-10 sm:px-8">
    <section id="workspaces" class="overflow-hidden rounded-2xl bg-white shadow-sm ring-1 ring-slate-200">
      <div class="flex items-center justify-between border-b border-slate-100 px-6 py-4">
        <div class="flex items-center gap-3"><h2 class="font-semibold text-ink">Workspaces</h2><span class="text-sm text-slate-500">{{len .Workspaces}} total</span></div>
        <div class="relative inline-flex rounded-md shadow-sm">
          <a class="inline-flex items-center rounded-l-md bg-indigo-600 px-3 py-2 text-sm font-semibold text-white transition hover:bg-indigo-500 focus:z-10 focus:outline-none focus:ring-2 focus:ring-indigo-600 focus:ring-offset-2" href="/w/?t={{.DefaultTemplate}}">Create workspace</a>
          <details class="group relative">
            <summary class="flex h-full cursor-pointer list-none items-center rounded-r-md border-l border-indigo-400 bg-indigo-600 px-2 text-white transition hover:bg-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-600 focus:ring-offset-2"><span class="sr-only">Choose workspace template</span><svg class="size-4" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M5.23 7.21a.75.75 0 0 1 1.06.02L10 11.17l3.71-3.94a.75.75 0 1 1 1.09 1.04l-4.25 4.5a.75.75 0 0 1-1.09 0l-4.25-4.5a.75.75 0 0 1 .02-1.08Z" clip-rule="evenodd" /></svg></summary>
            <div class="absolute right-0 z-10 mt-2 w-56 overflow-hidden rounded-lg bg-white py-1 shadow-lg ring-1 ring-black/5">
              {{range .Templates}}<a class="block px-4 py-2 text-sm text-slate-700 transition hover:bg-slate-50 hover:text-ink" href="/w/?t={{.Name}}">Create with {{.Name}}{{if .Default}} <span class="text-slate-400">(default)</span>{{end}}</a>{{end}}
              <a class="mt-1 block border-t border-slate-100 px-4 py-2 text-sm font-medium text-brand transition hover:bg-slate-50" href="/templates">View templates</a>
            </div>
          </details>
        </div>
      </div>
      <ul class="divide-y divide-slate-100">
        {{range .Workspaces}}
        <li class="flex flex-col gap-4 px-6 py-5 sm:flex-row sm:items-center sm:justify-between">
          <div class="min-w-0">
            <a class="font-semibold text-brand hover:text-brand-dark" href="/w/{{.ID}}/">{{if .DisplayName}}{{.DisplayName}}{{else}}{{.ID}}{{end}}</a>
            <p class="mt-1 text-sm text-slate-500">{{.ID}} · {{.TemplateName}}{{if .RepositoryFullName}} · {{.RepositoryFullName}}{{end}}</p>
          </div>
          <div class="flex flex-wrap items-center gap-2">
            {{with index $.TemplateResources .TemplateName}}<span class="text-xs text-slate-500">{{.CPUs}} CPU · {{.MemoryGiB}} GiB</span>{{end}}
            <span class="rounded-full bg-indigo-50 px-3 py-1 text-xs font-semibold text-indigo-700">{{.ObservedState}}</span>
            <div class="flex items-center gap-2 border-l border-slate-200 pl-3">
              {{if or (eq .ObservedState "pending") (eq .ObservedState "starting") (eq .ObservedState "stopping") (eq .ObservedState "deleting")}}
              <span class="text-sm font-medium text-slate-500">Transitioning</span>
              {{else if eq .ObservedState "running"}}
              <a class="inline-flex items-center justify-center rounded-md bg-indigo-600 px-3 py-2 text-sm font-semibold text-white shadow-sm transition hover:bg-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-600 focus:ring-offset-2" href="/w/{{.ID}}/">Open</a>
              <form method="post" action="/api/workspaces/{{.ID}}/stop"><input type="hidden" name="csrf_token" value="{{$.CSRF}}"><button class="inline-flex items-center justify-center rounded-md bg-white px-3 py-2 text-sm font-semibold text-slate-700 shadow-sm ring-1 ring-inset ring-slate-300 transition hover:bg-slate-50 focus:outline-none focus:ring-2 focus:ring-slate-500 focus:ring-offset-2">Stop</button></form>
              {{else if eq .DesiredState "stopped"}}
              <form method="post" action="/api/workspaces/{{.ID}}/stop"><input type="hidden" name="csrf_token" value="{{$.CSRF}}"><button class="inline-flex items-center justify-center rounded-md bg-white px-3 py-2 text-sm font-semibold text-slate-700 shadow-sm ring-1 ring-inset ring-slate-300 transition hover:bg-slate-50 focus:outline-none focus:ring-2 focus:ring-slate-500 focus:ring-offset-2">{{if eq .ObservedState "failed"}}Retry stop{{else}}Stopping{{end}}</button></form>
              {{else}}
              <form method="post" action="/api/workspaces/{{.ID}}/start"><input type="hidden" name="csrf_token" value="{{$.CSRF}}"><button class="inline-flex items-center justify-center rounded-md bg-emerald-600 px-3 py-2 text-sm font-semibold text-white shadow-sm transition hover:bg-emerald-500 focus:outline-none focus:ring-2 focus:ring-emerald-600 focus:ring-offset-2">{{if eq .ObservedState "failed"}}Retry{{else}}Start{{end}}</button></form>
              {{end}}
              <form method="post" action="/api/workspaces/{{.ID}}/delete" onsubmit="return confirm('Delete this workspace?')"><input type="hidden" name="csrf_token" value="{{$.CSRF}}"><button class="inline-flex items-center justify-center rounded-md bg-white px-3 py-2 text-sm font-semibold text-rose-700 shadow-sm ring-1 ring-inset ring-slate-300 transition hover:bg-rose-50 focus:outline-none focus:ring-2 focus:ring-rose-600 focus:ring-offset-2">Delete</button></form>
            </div>
          </div>
        </li>
        {{else}}
        <li class="px-6 py-14 text-center">
          <p class="font-medium text-ink">No workspaces yet</p>
          <p class="mt-1 text-sm text-slate-500">Create an empty workspace or open a GitHub repository URL.</p>
        </li>
        {{end}}
      </ul>
    </section>
  </main>
</body>
</html>`
	type templateOption struct {
		Name    string
		Default bool
	}
	options := make([]templateOption, 0, len(r.templates.Names))
	for name := range r.templates.Names {
		options = append(options, templateOption{Name: name, Default: name == r.templates.Default})
	}
	sort.Slice(options, func(left, right int) bool { return options[left].Name < options[right].Name })
	template.Must(template.New("dashboard").Parse(page)).Execute(w, map[string]any{"User": u, "Workspaces": ws, "Templates": options, "TemplateResources": r.templates.Resources, "DefaultTemplate": r.templates.Default, "CSRF": r.auth.CSRF(req)})
}

func (r *Router) templatesPage(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	type templateOption struct {
		Name           string
		Default        bool
		RequiredLabels []string
		Resources      workspace.Resources
	}
	options := make([]templateOption, 0, len(r.templates.Names))
	for name := range r.templates.Names {
		options = append(options, templateOption{
			Name:           name,
			Default:        name == r.templates.Default,
			RequiredLabels: append([]string(nil), r.templates.RequiredLabels[name]...),
			Resources:      r.templates.Resources[name],
		})
	}
	sort.Slice(options, func(left, right int) bool { return options[left].Name < options[right].Name })
	const page = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Templates · Quickworks</title>
  <link rel="stylesheet" href="/static/app.css">
</head>
<body>
  <nav class="border-b border-slate-200 bg-white">
	    <div class="mx-auto flex h-16 w-full items-center justify-between px-5 sm:px-8 lg:px-12">
	      <a class="text-lg font-semibold tracking-tight text-ink" href="/">Quickworks</a>
	      <div class="flex items-center gap-6 text-sm"><a class="font-medium text-slate-600 transition hover:text-ink" href="/">Workspaces</a><a class="font-medium text-ink" href="/templates" aria-current="page">Templates</a><span class="rounded-full bg-slate-100 px-3 py-1.5 font-medium text-slate-600">{{.User.GitHubLogin}}</span></div>
    </div>
  </nav>
  <main class="mx-auto min-h-screen max-w-5xl px-5 py-10 sm:px-8">
    <div class="flex items-end justify-between gap-4">
      <div><p class="text-sm font-medium text-brand">Workspace configuration</p><h1 class="mt-1 text-2xl font-semibold tracking-tight text-ink">Templates</h1><p class="mt-2 text-sm text-slate-500">Choose a template when creating a workspace. Each template is scheduled only to provisioners with its required labels.</p></div>
      <a class="inline-flex shrink-0 items-center rounded-md bg-indigo-600 px-3 py-2 text-sm font-semibold text-white shadow-sm transition hover:bg-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-600 focus:ring-offset-2" href="/w/?t={{.DefaultTemplate}}">Create workspace</a>
    </div>
    <section class="mt-8 overflow-hidden rounded-2xl bg-white shadow-sm ring-1 ring-slate-200">
      <ul class="divide-y divide-slate-100">
        {{range .Templates}}
        <li class="flex flex-col gap-4 px-6 py-5 sm:flex-row sm:items-center sm:justify-between">
          <div><div class="flex items-center gap-2"><h2 class="font-semibold text-ink">{{.Name}}</h2>{{if .Default}}<span class="rounded-full bg-indigo-50 px-2.5 py-1 text-xs font-medium text-indigo-700">Default</span>{{end}}</div><p class="mt-1 text-sm text-slate-500">{{.Resources.CPUs}} CPU · {{.Resources.MemoryGiB}} GiB memory</p><div class="mt-3 flex flex-wrap gap-2">{{range .RequiredLabels}}<span class="rounded-full bg-slate-100 px-2.5 py-1 text-xs font-medium text-slate-600">{{.}}</span>{{else}}<span class="text-xs text-slate-500">No provisioner labels required</span>{{end}}</div></div>
          <a class="inline-flex shrink-0 items-center justify-center rounded-md bg-white px-3 py-2 text-sm font-semibold text-slate-700 shadow-sm ring-1 ring-inset ring-slate-300 transition hover:bg-slate-50 focus:outline-none focus:ring-2 focus:ring-slate-500 focus:ring-offset-2" href="/w/?t={{.Name}}">Use template</a>
        </li>
        {{end}}
      </ul>
    </section>
  </main>
</body>
</html>`
	template.Must(template.New("templates").Parse(page)).Execute(w, map[string]any{"User": u, "Templates": options, "DefaultTemplate": r.templates.Default})
}

func (r *Router) me(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	writeJSON(w, 200, u)
}
func (r *Router) list(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	ws, err := r.workspaces.List(req.Context(), u.ID)
	if err != nil {
		http.Error(w, "list", 500)
		return
	}
	writeJSON(w, 200, ws)
}

func (r *Router) readiness(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	ws, err := r.workspaces.Get(req.Context(), u.ID, chi.URLParam(req, "id"))
	if err != nil {
		http.NotFound(w, req)
		return
	}
	_, err = r.agents.ActiveAgent(req.Context(), ws.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"observed_state": ws.ObservedState,
		"ready":          err == nil,
	})
}

func (r *Router) quotas(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	usage, err := r.workspaces.Usage(req.Context(), u.ID)
	if err != nil {
		http.Error(w, "read quotas", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

func (r *Router) provisioners(w http.ResponseWriter, req *http.Request) {
	type provisioner struct {
		WorkerID     string    `json:"worker_id"`
		LabelsJSON   string    `json:"-"`
		Labels       []string  `json:"labels"`
		Capacity     int       `json:"capacity"`
		ActiveBuilds int       `json:"active_builds"`
		LastSeenAt   time.Time `json:"last_seen_at"`
		Healthy      bool      `json:"healthy"`
	}
	var rows []provisioner
	if err := r.db.WithContext(req.Context()).Raw("SELECT worker_id, labels_json, capacity, active_builds, last_seen_at FROM provisioners ORDER BY worker_id").Scan(&rows).Error; err != nil {
		http.Error(w, "list provisioners", http.StatusInternalServerError)
		return
	}
	deadline := time.Now().Add(-90 * time.Second)
	for index := range rows {
		if err := json.Unmarshal([]byte(rows[index].LabelsJSON), &rows[index].Labels); err != nil {
			rows[index].Labels = nil
		}
		rows[index].Healthy = rows[index].LastSeenAt.After(deadline)
	}
	writeJSON(w, http.StatusOK, rows)
}

func (r *Router) templateAvailability(w http.ResponseWriter, req *http.Request) {
	type provisioner struct {
		LabelsJSON   string
		Capacity     int
		ActiveBuilds int
		LastSeenAt   time.Time
	}
	type availability struct {
		Name             string   `json:"name"`
		Default          bool     `json:"default"`
		RequiredLabels   []string `json:"required_labels"`
		MatchingWorkers  int      `json:"matching_workers"`
		AvailableWorkers int      `json:"available_workers"`
		AvailableSlots   int      `json:"available_slots"`
	}
	var provisioners []provisioner
	if err := r.db.WithContext(req.Context()).Raw("SELECT labels_json, capacity, active_builds, last_seen_at FROM provisioners").Scan(&provisioners).Error; err != nil {
		http.Error(w, "list template availability", http.StatusInternalServerError)
		return
	}
	result := make([]availability, 0, len(r.templates.Names))
	deadline := time.Now().Add(-90 * time.Second)
	for name := range r.templates.Names {
		item := availability{Name: name, Default: name == r.templates.Default, RequiredLabels: append([]string(nil), r.templates.RequiredLabels[name]...)}
		for _, worker := range provisioners {
			var labels []string
			if json.Unmarshal([]byte(worker.LabelsJSON), &labels) != nil || !labelsContain(labels, item.RequiredLabels) {
				continue
			}
			item.MatchingWorkers++
			if worker.LastSeenAt.After(deadline) && worker.Capacity > worker.ActiveBuilds {
				item.AvailableWorkers++
				item.AvailableSlots += worker.Capacity - worker.ActiveBuilds
			}
		}
		result = append(result, item)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	writeJSON(w, http.StatusOK, result)
}
func (r *Router) create(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	var input struct {
		Name       string `json:"name"`
		Template   string `json:"template"`
		Repository string `json:"repository"`
	}
	jsonRequest := strings.HasPrefix(req.Header.Get("Content-Type"), "application/json")
	if jsonRequest {
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&input); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
	} else {
		if err := req.ParseForm(); err != nil {
			http.Error(w, "invalid form", 400)
			return
		}
		input.Name = req.Form.Get("name")
		input.Template = req.Form.Get("template")
		input.Repository = req.Form.Get("repository")
	}
	templateName, err := r.template(input.Template)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var source *workspace.Repository
	var cloneURL string
	if input.Repository != "" {
		repository, err := r.resolveRepository(req.Context(), u.ID, input.Repository)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		source = &workspace.Repository{ID: repository.ID, FullName: repository.FullName}
		cloneURL = repository.CloneURL
	}
	key := req.Header.Get("Idempotency-Key")
	requestHash := hash(input.Name + "\x00" + templateName + "\x00" + input.Repository)
	ws, b, err := r.workspaces.CreateRepositoryIdempotent(req.Context(), u.ID, key, requestHash, input.Name, templateName, source)
	if err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	if cloneURL != "" {
		if err := r.auth.CreateCloneCredential(req.Context(), ws.ID, b.ID, u.ID, cloneURL); err != nil {
			http.Error(w, "prepare repository clone", http.StatusInternalServerError)
			return
		}
	}
	if !jsonRequest {
		http.Redirect(w, req, "/w/"+ws.ID+"/", http.StatusSeeOther)
		return
	}
	writeJSON(w, 202, map[string]any{"workspace": ws, "build": b})
}

func (r *Router) repository(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	name := chi.URLParam(req, "owner") + "/" + chi.URLParam(req, "repo")
	repository, err := r.resolveRepository(req.Context(), u.ID, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	const page = `<!doctype html><title>Open repository · Quickworks</title><h1>Open repository</h1><p>{{.Repository.FullName}}</p><p>Default branch: {{.Repository.DefaultBranch}}</p><form method="post" action="/api/workspaces"><input type="hidden" name="csrf_token" value="{{.CSRF}}"><input type="hidden" name="repository" value="{{.Repository.FullName}}"><label>Workspace name <input name="name" value="{{.Repository.FullName}}"></label><button>Create workspace</button></form>`
	template.Must(template.New("repository").Parse(page)).Execute(w, map[string]any{"Repository": repository, "CSRF": r.auth.CSRF(req)})
}

func (r *Router) resolveRepository(ctx context.Context, userID int64, value string) (auth.Repository, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return auth.Repository{}, errors.New("repository must use owner/name")
	}
	return r.auth.Repository(ctx, userID, parts[0], parts[1])
}

func (r *Router) template(name string) (string, error) {
	if name == "" {
		return r.templates.Default, nil
	}
	if !r.templates.Names[name] {
		return "", errors.New("unknown workspace template")
	}
	return name, nil
}
func (r *Router) transition(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		u, _ := auth.UserFromContext(req.Context())
		b, err := r.workspaces.Transition(req.Context(), u.ID, chi.URLParam(req, "id"), kind)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				http.Error(w, "not found", 404)
			} else {
				http.Error(w, err.Error(), 409)
			}
			return
		}
		if strings.Contains(req.Header.Get("Accept"), "text/html") {
			if req.FormValue("return_to") == "workbench" {
				http.Redirect(w, req, "/w/"+chi.URLParam(req, "id")+"/", http.StatusSeeOther)
				return
			}
			http.Redirect(w, req, "/", http.StatusSeeOther)
			return
		}
		writeJSON(w, 202, b)
	}
}

func (r *Router) builds(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	builds, err := r.workspaces.Builds(req.Context(), u.ID, chi.URLParam(req, "id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.NotFound(w, req)
			return
		}
		http.Error(w, "list builds", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, builds)
}

func (r *Router) logs(w http.ResponseWriter, req *http.Request) {
	u, _ := auth.UserFromContext(req.Context())
	buildID := chi.URLParam(req, "id")
	owned, err := r.workspaces.OwnsBuild(req.Context(), u.ID, buildID)
	if err != nil || !owned {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sequence := 0
	for {
		var entries []struct {
			Sequence  int       `json:"sequence"`
			Timestamp time.Time `json:"timestamp"`
			Level     string    `json:"level"`
			Message   string    `json:"message"`
		}
		if err := r.db.WithContext(req.Context()).Raw("SELECT sequence, timestamp, level, message FROM build_logs WHERE build_id = ? AND sequence > ? ORDER BY sequence", buildID, sequence).Scan(&entries).Error; err != nil {
			return
		}
		for _, entry := range entries {
			payload, _ := json.Marshal(entry)
			_, _ = fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", entry.Sequence, payload)
			sequence = entry.Sequence
		}
		flusher.Flush()
		select {
		case <-req.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (r *Router) internal(req *http.Request, workerID string) bool {
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	expected, ok := r.provisionerTokenHash[workerID]
	return token != "" && ok && hash(token) == expected
}
func (r *Router) claim(w http.ResponseWriter, req *http.Request) {
	var in struct {
		WorkerID string   `json:"worker_id"`
		Labels   []string `json:"labels"`
		Capacity int      `json:"capacity"`
	}
	if json.NewDecoder(req.Body).Decode(&in) != nil || in.WorkerID == "" || !validLabels(in.Labels) {
		http.Error(w, "worker_id required", 400)
		return
	}
	if !r.internal(req, in.WorkerID) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if in.Capacity == 0 {
		in.Capacity = 1
	}
	if in.Capacity < 1 || in.Capacity > 100 {
		http.Error(w, "capacity must be between 1 and 100", http.StatusBadRequest)
		return
	}
	if err := r.expireLeases(req.Context()); err != nil {
		http.Error(w, "recover expired leases", http.StatusInternalServerError)
		return
	}
	labelsJSON, _ := json.Marshal(in.Labels)
	if err := r.db.WithContext(req.Context()).Exec("INSERT INTO provisioners(worker_id, labels_json, capacity, active_builds, last_seen_at, updated_at) VALUES (?, ?, ?, 0, ?, ?) ON CONFLICT(worker_id) DO UPDATE SET labels_json = excluded.labels_json, capacity = excluded.capacity, last_seen_at = excluded.last_seen_at, updated_at = excluded.updated_at", in.WorkerID, string(labelsJSON), in.Capacity, time.Now(), time.Now()).Error; err != nil {
		http.Error(w, "register provisioner", 500)
		return
	}
	lease := workspace.RandomID()
	until := time.Now().Add(2 * time.Minute)
	var b workspace.Build
	err := r.claimMatchingBuild(req, in.WorkerID, in.Labels, in.Capacity, lease, until, &b)
	if err != nil {
		http.Error(w, "claim", 500)
		return
	}
	if b.ID == "" {
		r.recordSchedulerEvent(req.Context(), in.WorkerID, "lease_empty", "", "", "", "no queued build matched this worker labels and capacity")
		w.WriteHeader(204)
		return
	}
	r.recordSchedulerEvent(req.Context(), in.WorkerID, "build_claimed", b.ID, b.WorkspaceID, b.TemplateName, "build lease issued")
	response := map[string]any{"build": b, "lease_id": lease, "lease_expires_at": until}
	if b.Transition == "start" {
		enrollment, err := r.agents.CreateEnrollment(req.Context(), b.WorkspaceID, b.ID)
		if err != nil {
			http.Error(w, "create agent enrollment", http.StatusInternalServerError)
			return
		}
		response["agent"] = enrollment
	}
	writeJSON(w, 200, response)
}

func (r *Router) recordSchedulerEvent(ctx context.Context, workerID, eventType, buildID, workspaceID, templateName, detail string) {
	_ = r.db.WithContext(ctx).Exec("INSERT INTO scheduler_events(worker_id, event_type, build_id, workspace_id, template_name, detail) VALUES (?, ?, ?, ?, ?, ?)", workerID, eventType, nilIfEmpty(buildID), nilIfEmpty(workspaceID), nilIfEmpty(templateName), detail).Error
}

func (r *Router) schedulerEvents(w http.ResponseWriter, req *http.Request) {
	var events []struct {
		ID           int64     `json:"id"`
		WorkerID     string    `json:"worker_id"`
		EventType    string    `json:"event_type"`
		BuildID      *string   `json:"build_id,omitempty"`
		WorkspaceID  *string   `json:"workspace_id,omitempty"`
		TemplateName *string   `json:"template_name,omitempty"`
		Detail       string    `json:"detail"`
		CreatedAt    time.Time `json:"created_at"`
	}
	if err := r.db.WithContext(req.Context()).Raw("SELECT id, worker_id, event_type, build_id, workspace_id, template_name, detail, created_at FROM scheduler_events ORDER BY id DESC LIMIT 200").Scan(&events).Error; err != nil {
		http.Error(w, "list scheduler events", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// expireLeases never requeues an unknown OpenTofu operation. An expired worker
// might still be applying infrastructure, so the build is failed for explicit
// operator/user retry after reconciliation rather than being assigned twice.
func (r *Router) expireLeases(ctx context.Context) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var expired []workspace.Build
		if err := tx.Where("status = 'running' AND lease_expires_at <= ?", time.Now()).Find(&expired).Error; err != nil {
			return err
		}
		for _, build := range expired {
			message := "provisioner lease expired; infrastructure reconciliation is required before retry"
			result := tx.Model(&workspace.Build{}).Where("id = ? AND status = 'running'", build.ID).Updates(map[string]any{"status": "failed", "error": message, "completed_at": time.Now()})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			if err := tx.Model(&workspace.Workspace{}).Where("id = ?", build.WorkspaceID).Updates(map[string]any{"observed_state": "failed", "updated_at": time.Now()}).Error; err != nil {
				return err
			}
			if build.ClaimedBy != nil {
				if err := tx.Exec("UPDATE provisioners SET active_builds = CASE WHEN active_builds > 0 THEN active_builds - 1 ELSE 0 END, updated_at = ? WHERE worker_id = ?", time.Now(), *build.ClaimedBy).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (r *Router) claimMatchingBuild(req *http.Request, workerID string, labels []string, capacity int, lease string, until time.Time, claimed *workspace.Build) error {
	return r.db.WithContext(req.Context()).Transaction(func(tx *gorm.DB) error {
		var active int64
		if err := tx.Model(&workspace.Build{}).Where("claimed_by = ? AND status = 'running'", workerID).Count(&active).Error; err != nil {
			return err
		}
		if active >= int64(capacity) {
			return nil
		}
		var candidates []workspace.Build
		if err := tx.Where("status = 'queued'").Order("created_at, id").Find(&candidates).Error; err != nil {
			return err
		}
		for _, candidate := range candidates {
			if !labelsContain(labels, r.templates.RequiredLabels[candidate.TemplateName]) {
				continue
			}
			result := tx.Raw(`UPDATE workspace_builds SET status='running', claimed_by=?, lease_id_hash=?, lease_expires_at=?, started_at=? WHERE id=? AND status='queued' RETURNING *`, workerID, hash(lease), until, time.Now(), candidate.ID).Scan(claimed)
			if result.Error != nil {
				return result.Error
			}
			if claimed.ID != "" {
				return tx.Exec("UPDATE provisioners SET active_builds = active_builds + 1, last_seen_at = ?, updated_at = ? WHERE worker_id = ?", time.Now(), time.Now(), workerID).Error
			}
		}
		return nil
	})
}

func validLabels(labels []string) bool {
	seen := make(map[string]bool)
	for _, label := range labels {
		if !labelPattern.MatchString(label) || seen[label] {
			return false
		}
		seen[label] = true
	}
	return true
}

func labelsContain(have, required []string) bool {
	set := make(map[string]bool, len(have))
	for _, label := range have {
		set[label] = true
	}
	for _, label := range required {
		if !set[label] {
			return false
		}
	}
	return true
}

func (r *Router) enroll(w http.ResponseWriter, req *http.Request) {
	var input struct {
		EnrollmentToken string `json:"enrollment_token"`
		PublicKey       string `json:"public_key"`
	}
	if err := json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&input); err != nil {
		http.Error(w, "invalid agent enrollment", http.StatusBadRequest)
		return
	}
	publicKey, err := base64.RawStdEncoding.DecodeString(input.PublicKey)
	if err != nil {
		http.Error(w, "invalid agent public key", http.StatusBadRequest)
		return
	}
	registration, err := r.agents.Register(req.Context(), input.EnrollmentToken, publicKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	clone, err := r.auth.ConsumeCloneCredential(req.Context(), registration.WorkspaceID, registration.BuildID)
	if err != nil {
		http.Error(w, "read clone credential", http.StatusInternalServerError)
		return
	}
	var owner struct{ OwnerID int64 }
	if err := r.db.WithContext(req.Context()).Raw("SELECT owner_id FROM workspaces WHERE id = ? AND deleted_at IS NULL", registration.WorkspaceID).Scan(&owner).Error; err != nil || owner.OwnerID == 0 {
		http.Error(w, "read workspace owner", http.StatusInternalServerError)
		return
	}
	githubToken, err := r.auth.GitHubToken(req.Context(), owner.OwnerID)
	if err != nil {
		http.Error(w, "read GitHub credential", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"workspace_id":            registration.WorkspaceID,
		"build_id":                registration.BuildID,
		"agent_id":                registration.AgentID,
		"session":                 registration.Session,
		"workbench_version":       r.workbenchConfig.Version,
		"workbench_bundle_url":    r.workbenchConfig.BundleURL,
		"workbench_bundle_sha256": r.workbenchConfig.BundleSHA256,
		"workbench_entrypoint":    r.workbenchConfig.Entrypoint,
		"workbench_byok_config":   string(r.workbenchConfig.BYOKConfig),
		"workbench_environment":   string(r.workbenchConfig.Environment),
		"repository_url":          clone.RepositoryURL,
		"clone_token":             clone.Token,
		"github_token":            githubToken,
	})
}

func (r *Router) agentConnect(w http.ResponseWriter, req *http.Request) {
	agentID := req.URL.Query().Get("agent_id")
	session := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if err := r.agents.AuthenticateSession(req.Context(), agentID, session); err != nil {
		http.Error(w, "agent authentication failed", http.StatusUnauthorized)
		return
	}
	connection, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer connection.Close(websocket.StatusNormalClosure, "agent disconnected")
	stream := websocket.NetConn(req.Context(), connection, websocket.MessageBinary)
	muxSession, err := yamux.Server(stream, nil)
	if err != nil {
		return
	}
	defer r.tcpTunnels.Disconnect(agentID, muxSession)
	r.tcpTunnels.Connect(agentID, muxSession)
	_ = r.agents.Heartbeat(req.Context(), agentID)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case <-muxSession.CloseChan():
			return
		case <-ticker.C:
			_ = r.agents.Heartbeat(req.Context(), agentID)
		}
	}
}

func (r *Router) agentReady(w http.ResponseWriter, req *http.Request) {
	var input struct {
		AgentID string `json:"agent_id"`
	}
	if json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&input) != nil || input.AgentID == "" {
		http.Error(w, "agent_id required", http.StatusBadRequest)
		return
	}
	session := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if err := r.agents.AuthenticateSession(req.Context(), input.AgentID, session); err != nil {
		http.Error(w, "agent authentication failed", http.StatusUnauthorized)
		return
	}
	if err := r.agents.MarkReady(req.Context(), input.AgentID); err != nil {
		http.Error(w, "mark agent ready", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func requestScheme(req *http.Request) string {
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

func safeHeaders(headers http.Header) http.Header {
	result := make(http.Header)
	for key, values := range headers {
		canonical := http.CanonicalHeaderKey(key)
		if canonical == "Connection" || canonical == "Upgrade" || canonical == "Keep-Alive" || canonical == "Proxy-Authorization" || canonical == "Proxy-Connection" || canonical == "Te" || canonical == "Trailer" || canonical == "Transfer-Encoding" || canonical == "X-Forwarded-For" || canonical == "X-Forwarded-Host" || canonical == "X-Forwarded-Proto" {
			continue
		}
		result[canonical] = append([]string(nil), values...)
	}
	return result
}
func (r *Router) lease(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	action := chi.URLParam(req, "action")
	var in struct {
		LeaseID  string `json:"lease_id"`
		WorkerID string `json:"worker_id"`
		Error    string `json:"error"`
		Level    string `json:"level"`
		Message  string `json:"message"`
	}
	if json.NewDecoder(req.Body).Decode(&in) != nil || in.LeaseID == "" || in.WorkerID == "" {
		http.Error(w, "lease_id and worker_id required", 400)
		return
	}
	if !r.internal(req, in.WorkerID) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.db.WithContext(req.Context()).Where("id = ? AND claimed_by = ? AND lease_id_hash = ? AND lease_expires_at > ?", id, in.WorkerID, hash(in.LeaseID), time.Now())
	switch action {
	case "renew":
		result := q.Model(&workspace.Build{}).Update("lease_expires_at", time.Now().Add(2*time.Minute))
		if result.Error != nil || result.RowsAffected != 1 {
			http.Error(w, "lease expired", 409)
			return
		}
		w.WriteHeader(204)
	case "complete":
		status := "succeeded"
		if in.Error != "" {
			status = "failed"
		}
		if err := r.completeBuild(req, id, in.WorkerID, in.LeaseID, status, in.Error); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(204)
	case "logs":
		if in.Message == "" {
			http.Error(w, "message required", 400)
			return
		}
		var active workspace.Build
		if err := q.First(&active).Error; err != nil {
			http.Error(w, "lease expired", http.StatusConflict)
			return
		}
		var n int
		r.db.Raw("SELECT COALESCE(MAX(sequence),0)+1 FROM build_logs WHERE build_id = ?", id).Scan(&n)
		if err := r.db.Exec("INSERT INTO build_logs(build_id,sequence,level,message) VALUES(?,?,?,?)", id, n, in.Level, in.Message).Error; err != nil {
			http.Error(w, "log", 500)
			return
		}
		w.WriteHeader(204)
	default:
		http.NotFound(w, req)
	}
}

func (r *Router) completeBuild(req *http.Request, id, workerID, leaseID, status, buildError string) error {
	return r.db.WithContext(req.Context()).Transaction(func(tx *gorm.DB) error {
		var build workspace.Build
		if err := tx.Where("id = ? AND status = 'running' AND claimed_by = ? AND lease_id_hash = ? AND lease_expires_at > ?", id, workerID, hash(leaseID), time.Now()).First(&build).Error; err != nil {
			return errors.New("lease expired")
		}
		if err := tx.Model(&workspace.Build{}).Where("id = ?", id).Updates(map[string]any{"status": status, "error": nilIfEmpty(buildError), "completed_at": time.Now()}).Error; err != nil {
			return err
		}
		observed := "failed"
		if status == "succeeded" {
			observed = map[string]string{"start": "running", "stop": "stopped", "delete": "deleted"}[build.Transition]
		}
		updates := map[string]any{"observed_state": observed, "updated_at": time.Now()}
		if status == "succeeded" && build.Transition == "delete" {
			updates["deleted_at"] = time.Now()
		}
		if err := tx.Model(&workspace.Workspace{}).Where("id = ?", build.WorkspaceID).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Exec("UPDATE provisioners SET active_builds = CASE WHEN active_builds > 0 THEN active_builds - 1 ELSE 0 END, updated_at = ? WHERE worker_id = ?", time.Now(), workerID).Error
	})
}
func nilIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", workspace.RandomID())
		next.ServeHTTP(w, r)
	})
}
