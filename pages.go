package portkit

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// page is a help or error page, rendered as HTML for browsers and as plain
// text otherwise.
type page struct {
	kind   string // the X-Portkit header; the HTML reloads once it changes
	title  string
	blocks []block
}

type block struct {
	text  string   // a paragraph
	cmds  []string // commands to run
	pre   string   // log output
	links []string // hosts to link to
	tight bool     // no blank line before it in text
}

// HelpHandler serves the page for names that aren't registered.
type HelpHandler struct {
	app *App
}

func (HelpHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.portkit_help",
		New: func() caddy.Module { return new(HelpHandler) },
	}
}

func (h *HelpHandler) Provision(ctx caddy.Context) (err error) {
	h.app, err = loadApp(ctx)
	return err
}

func (h *HelpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	name, ok := nameFromHost(r.Host)
	if !ok {
		return servePage(w, r, http.StatusNotFound, page{
			kind:  "unregistered",
			title: "portkit serves only names under .localhost.",
		})
	}
	return servePage(w, r, http.StatusNotFound, helpPage(name, h.app.Services))
}

func helpPage(name string, services map[string]*Service) page {
	host := name + ".localhost"
	p := page{kind: "unregistered", title: host + " isn't registered with portkit."}

	if _, _, svc := lookup(services, host); svc != nil && svc.Owner != nil && !svc.Owner.alive() {
		how := "with `portkit run`"
		if svc.Mode == ModeAdopted {
			how = fmt.Sprintf("by process %d with `portkit adopt`", svc.Owner.PID)
		}
		p.blocks = append(p.blocks, block{text: fmt.Sprintf("%s was last run %s, which has exited.", name, how)})
		owners.remove(name, svc.Owner)
	}

	p.blocks = append(p.blocks,
		block{text: "Run one of these from your project's directory:"},
		block{text: "Proxy to a server already running on a fixed port:", cmds: []string{"portkit register " + name + " 3000"}},
		block{text: "Start a command whenever " + host + " is visited:", cmds: []string{"portkit register " + name + " -- npm run dev -- --port {port}"}},
		block{text: "Run a command now, in your terminal, until it exits:", cmds: []string{"portkit run " + name + " -- npm run dev"}},
	)

	// A parent that is a registered service: list its subdomains.
	if _, parent, found := strings.Cut(name, "."); found {
		if svc := services[parent]; svc != nil && len(svc.Subdomains) > 0 {
			var subs []string
			for sub := range svc.Subdomains {
				subs = append(subs, sub+"."+parent+".localhost")
			}
			sort.Strings(subs)
			p.blocks = append(p.blocks, block{text: parent + ".localhost has these subdomains:", links: subs})
		}
	}

	// Services registered under this name, such as instances of one tool.
	var under []string
	for _, n := range sortedNames(services) {
		if strings.HasSuffix(n, "."+name) {
			under = append(under, n+".localhost")
		}
	}
	if len(under) > 0 {
		p.blocks = append(p.blocks, block{text: "Services under " + host + ":", links: under})
	}

	var close []string
	for _, n := range sortedNames(services) {
		if n != name && editDistance(n, name) <= 2 {
			close = append(close, n+".localhost")
		}
	}
	if len(close) > 0 {
		p.blocks = append(p.blocks, block{text: "Did you mean " + strings.Join(close, " or ") + "?"})
	}
	if all := sortedNames(services); len(all) > 0 {
		var hosts []string
		for _, n := range all {
			hosts = append(hosts, n+".localhost")
		}
		p.blocks = append(p.blocks, block{text: "Registered:", links: hosts, tight: len(close) > 0})
	}
	return p
}

// crashPage is shown when an on-demand service keeps exiting or never
// opened a port.
func crashPage(host, name string, ce *CrashError) page {
	p := page{kind: "unavailable", title: host + ".localhost couldn't start."}
	p.blocks = append(p.blocks, block{text: ce.Message})
	if ce.Tail != "" {
		p.blocks = append(p.blocks, block{text: "The last lines of its log:", pre: ce.Tail})
	}
	p.blocks = append(p.blocks, block{
		text: "Fix the problem, then let portkit try again:",
		cmds: []string{"portkit stop " + name, "portkit logs " + name},
	})
	return p
}

// notListeningPage is shown when nothing accepts connections on a fixed,
// foreground or adopted service's port.
func notListeningPage(host, name string, svc *Service, port int) page {
	p := page{kind: "unavailable", title: host + ".localhost isn't responding."}
	p.blocks = append(p.blocks, block{text: fmt.Sprintf("It proxies to localhost:%d, but nothing is listening there. Start your server and this page reloads on its own.", port)})
	if svc.Mode == ModePort {
		p.blocks = append(p.blocks, block{
			text: "Or have portkit start it whenever " + host + ".localhost is visited:",
			cmds: []string{"portkit register " + name + " -- npm run dev -- --port {port}"},
		})
	}
	return p
}

// indexPage lists the registered services at https://portkit.localhost.
func indexPage(services map[string]*Service) page {
	p := page{kind: "index", title: "portkit"}
	if len(services) == 0 {
		p.blocks = append(p.blocks, block{text: "No services are registered.", cmds: []string{"portkit register foo 3000"}})
		return p
	}
	var hosts []string
	for _, n := range sortedNames(services) {
		hosts = append(hosts, n+".localhost")
	}
	p.blocks = append(p.blocks, block{text: "Registered:", links: hosts}, block{text: "Machine-readable list: https://portkit.localhost/services"})
	return p
}

func servePage(w http.ResponseWriter, r *http.Request, status int, p page) error {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Portkit", p.kind)
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		return pageHTML.Execute(w, p.htmlData())
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, err := w.Write([]byte(p.text()))
	return err
}

func (p page) text() string {
	var b strings.Builder
	b.WriteString(p.title);b.WriteString("\n")
	for i, bl := range p.blocks {
		// The first paragraph after the title sits right under it, as in a
		// heading; the rest are separated by blank lines.
		if (i > 0 || bl.cmds != nil || bl.pre != "") && !bl.tight {
			b.WriteString("\n")
		}
		if len(bl.links) > 0 {
			b.WriteString(bl.text);b.WriteString(" ");b.WriteString(strings.Join(bl.links, ", "));b.WriteString("\n")
			continue
		}
		if bl.text != "" {
			b.WriteString(bl.text);b.WriteString("\n")
		}
		for _, c := range bl.cmds {
			b.WriteString("  ");b.WriteString(c);b.WriteString("\n")
		}
		if bl.pre != "" {
			for l := range strings.SplitSeq(bl.pre, "\n") {
				b.WriteString("  | ");b.WriteString(l);b.WriteString("\n")
			}
		}
	}
	return b.String()
}

type htmlBlock struct {
	Text  string
	Cmds  []string
	Pre   string
	Links []string
}

func (p page) htmlData() map[string]any {
	blocks := make([]htmlBlock, len(p.blocks))
	for i, b := range p.blocks {
		blocks[i] = htmlBlock{Text: b.text, Cmds: b.cmds, Pre: b.pre, Links: b.links}
	}
	return map[string]any{"Kind": p.kind, "Title": p.title, "Blocks": blocks}
}

var pageHTML = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light dark; --fg: #1d1d1f; --muted: #6e6e73; --bg: #fbfbfd; --code: #f0f0f3; --link: #0a66d8; }
@media (prefers-color-scheme: dark) { :root { --fg: #f2f2f4; --muted: #a1a1a6; --bg: #141416; --code: #222226; --link: #6aa9ff; } }
body { margin: 0; background: var(--bg); color: var(--fg); font: 16px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
main { max-width: 44rem; margin: 0 auto; padding: 3rem 1rem; }
h1 { font-size: 1.4rem; margin: 0 0 1.5rem; }
p { margin: 1.25rem 0 0.4rem; }
code, pre { font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; }
pre { background: var(--code); padding: 0.6rem 0.8rem; border-radius: 6px; overflow-x: auto; margin: 0.25rem 0; }
pre.log { color: var(--muted); max-height: 24rem; }
a { color: var(--link); }
ul { margin: 0.25rem 0; padding-left: 1.25rem; }
footer { margin-top: 2.5rem; color: var(--muted); font-size: 0.85rem; }
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
{{range .Blocks}}
{{if .Text}}<p>{{.Text}}</p>{{end}}
{{range .Cmds}}<pre>{{.}}</pre>{{end}}
{{if .Pre}}<pre class="log">{{.Pre}}</pre>{{end}}
{{if .Links}}<ul>{{range .Links}}<li><a href="https://{{.}}">{{.}}</a></li>{{end}}</ul>{{end}}
{{end}}
<footer>portkit</footer>
</main>
{{if ne .Kind "index"}}
<script>
// Reload once this URL serves something other than this page, such as the
// app after ` + "`portkit register`" + `.
const kind = {{.Kind}};
(async () => {
  for (;;) {
    await new Promise((done) => setTimeout(done, 2000));
    try {
      const r = await fetch(location.href, { cache: "no-store", headers: { Accept: "text/plain" } });
      if (r.headers.get("X-Portkit") !== kind) return location.reload();
    } catch (e) {}
  }
})();
</script>
{{end}}
</body>
</html>
`))

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
