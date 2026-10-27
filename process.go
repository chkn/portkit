package portkit

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// FreePort picks a free port by listening on 127.0.0.1:0 and closing it.
func FreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// AssignPorts picks a free port for {port} ("") and for each placeholder
// subdomain.
func AssignPorts(svc *Service) (map[string]int, error) {
	ports := map[string]int{}
	used := map[int]bool{}
	pick := func(key string) error {
		for range 10 {
			p, err := FreePort()
			if err != nil {
				return err
			}
			if !used[p] {
				used[p] = true
				ports[key] = p
				return nil
			}
		}
		return errors.New("no free port")
	}
	if err := pick(""); err != nil {
		return nil, err
	}
	for sub, sd := range svc.Subdomains {
		if sd.Assign {
			if err := pick(sub); err != nil {
				return nil, err
			}
		}
	}
	return ports, nil
}

// Targets lists the ports detection has to find for svc.
func Targets(svc *Service, ports map[string]int) []Target {
	ts := []Target{{Name: "", Assigned: ports[""], Fixed: svc.ListenPort}}
	subs := make([]string, 0, len(svc.Subdomains))
	for sub := range svc.Subdomains {
		subs = append(subs, sub)
	}
	sort.Strings(subs)
	for _, sub := range subs {
		sd := svc.Subdomains[sub]
		ts = append(ts, Target{Name: sub, Assigned: ports[sub], Fixed: sd.Port, Match: sd.re})
	}
	return ts
}

// envName is the variable for a subdomain's port, e.g. PORTKIT_PORT_API.
func envName(sub string) string {
	return "PORTKIT_PORT_" + strings.ToUpper(strings.ReplaceAll(sub, "-", "_"))
}

// ChildEnv builds a child's environment: base, then the service's saved
// variables, then the variables portkit adds.
func ChildEnv(base []string, name string, svc *Service, ports map[string]int) []string {
	env := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := env[k]; !ok {
			order = append(order, k)
		}
		env[k] = v
	}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	for k, v := range svc.Env {
		set(k, v)
	}
	set("PORT", strconv.Itoa(ports[""]))
	set("PORTKIT_SERVICE", name)
	set("PORTKIT_URL", ServiceURL(name))
	set("PORTKIT_HOME", Home())
	for sub, sd := range svc.Subdomains {
		if p := ports[sub]; p != 0 {
			set(envName(sub), strconv.Itoa(p))
		} else if sd.Port != 0 {
			set(envName(sub), strconv.Itoa(sd.Port))
		}
	}
	if _, ok := env["NODE_EXTRA_CA_CERTS"]; !ok {
		set("NODE_EXTRA_CA_CERTS", CACertPath())
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+env[k])
	}
	return out
}

// lookPath finds file in the PATH from env, since Caddy's own PATH under
// launchd or systemd is minimal. Paths with a slash are relative to dir.
func lookPath(file string, env []string, dir string) (string, error) {
	if strings.Contains(file, "/") {
		p := file
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if isExecutable(p) {
			return p, nil
		}
		return "", fmt.Errorf("%s: not an executable file", file)
	}
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for d := range strings.SplitSeq(path, string(os.PathListSeparator)) {
		if d == "" {
			d = "."
		}
		if !filepath.IsAbs(d) {
			d = filepath.Join(dir, d)
		}
		if p := filepath.Join(d, file); isExecutable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: command not found in PATH=%s", file, path)
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&fs.ModePerm&0o111 != 0
}
