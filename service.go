package portkit

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Service modes.
const (
	ModePort       = "port"
	ModeOnDemand   = "on_demand"
	ModeForeground = "foreground"
	ModeAdopted    = "adopted"
)

const (
	defaultIdleTimeout = 30 * time.Minute
	maxLabels          = 4 // labels under .localhost, subdomain included
	reservedName       = "portkit"
)

// Service is one record in apps.portkit.services. The key is its name.
type Service struct {
	Mode    string            `json:"mode"`
	Port    int               `json:"port,omitempty"`
	Command []string          `json:"command,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`

	// Subdomains are served at <sub>.<name>.localhost.
	Subdomains map[string]*Subdomain `json:"subdomains,omitempty"`

	// IdleTimeout is a duration such as "30m", or "never".
	IdleTimeout string `json:"idle_timeout,omitempty"`

	// HostHeader replaces the Host header sent upstream, e.g. "localhost:{port}".
	HostHeader string `json:"host_header,omitempty"`

	// ListenPort skips port detection for a process whose port is known.
	ListenPort int `json:"listen_port,omitempty"`

	// Owner is the process a "foreground" or "adopted" record lives with.
	Owner *Owner `json:"owner,omitempty"`

	// Replaces is the record an adopter took the name from; it applies
	// again when the owner exits.
	Replaces *Service `json:"replaces,omitempty"`

	idleTimeout time.Duration // 0 means never
}

// Subdomain declares where a subdomain's port comes from: a placeholder in
// the command (Assign), a fixed Port, or the first output line that Matches.
type Subdomain struct {
	Assign bool   `json:"assign,omitempty"`
	Port   int    `json:"port,omitempty"`
	Match  string `json:"match,omitempty"`

	re *regexp.Regexp
}

// Owner identifies a process by ID and start time, so a reused ID doesn't
// count as the same process.
type Owner struct {
	PID     int    `json:"pid"`
	Started string `json:"started"`
}

var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidLabel reports whether s is a single lowercase DNS label.
func ValidLabel(s string) bool { return labelRe.MatchString(s) }

// ValidateName checks a service name: one or more DNS labels joined by dots.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("empty service name")
	}
	if name == reservedName {
		return fmt.Errorf("%q is reserved for portkit's own API", name)
	}
	labels := strings.Split(name, ".")
	if len(labels) > maxLabels {
		return fmt.Errorf("%q has %d labels; names under .localhost can have at most %d", name, len(labels), maxLabels)
	}
	for _, l := range labels {
		if !ValidLabel(l) {
			return fmt.Errorf("%q is not a valid name: each part must match [a-z0-9-]", name)
		}
	}
	return nil
}

// provision parses and validates the record.
func (s *Service) provision(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	switch s.Mode {
	case ModePort, ModeForeground, ModeAdopted:
		if s.Port <= 0 || s.Port > 65535 {
			return fmt.Errorf("service %s: mode %s needs a port", name, s.Mode)
		}
	case ModeOnDemand:
		if len(s.Command) == 0 {
			return fmt.Errorf("service %s: mode on_demand needs a command", name)
		}
	default:
		return fmt.Errorf("service %s: unknown mode %q", name, s.Mode)
	}
	if (s.Mode == ModeForeground || s.Mode == ModeAdopted) && s.Owner == nil {
		return fmt.Errorf("service %s: mode %s needs an owner", name, s.Mode)
	}
	switch s.IdleTimeout {
	case "":
		s.idleTimeout = defaultIdleTimeout
	case "never":
		s.idleTimeout = 0
	default:
		d, err := time.ParseDuration(s.IdleTimeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("service %s: bad idle_timeout %q", name, s.IdleTimeout)
		}
		s.idleTimeout = d
	}
	if len(s.Subdomains) > 0 && len(strings.Split(name, "."))+1 > maxLabels {
		return fmt.Errorf("service %s: subdomains would have more than %d labels", name, maxLabels)
	}
	for sub, sd := range s.Subdomains {
		if !ValidLabel(sub) || sub == "port" {
			return fmt.Errorf("service %s: bad subdomain name %q", name, sub)
		}
		if err := sd.provision(s.Mode); err != nil {
			return fmt.Errorf("service %s: subdomain %s: %w", name, sub, err)
		}
	}
	if s.Mode == ModeOnDemand {
		for _, ph := range Placeholders(s.Command) {
			if ph == "port" {
				continue
			}
			if sd := s.Subdomains[ph]; sd == nil || !sd.Assign {
				return fmt.Errorf("service %s: placeholder {%s} has no matching subdomain", name, ph)
			}
		}
	}
	if s.Replaces != nil {
		if err := s.Replaces.provision(name); err != nil {
			return err
		}
	}
	return nil
}

func (sd *Subdomain) provision(mode string) error {
	n := 0
	if sd.Assign {
		n++
	}
	if sd.Port != 0 {
		n++
	}
	if sd.Match != "" {
		n++
	}
	if n != 1 {
		return fmt.Errorf("needs exactly one of assign, port or match")
	}
	if sd.Port < 0 || sd.Port > 65535 {
		return fmt.Errorf("bad port %d", sd.Port)
	}
	if mode != ModeOnDemand && mode != ModeForeground && sd.Port == 0 {
		return fmt.Errorf("mode %s supports only fixed ports", mode)
	}
	if sd.Match != "" {
		re, err := regexp.Compile(sd.Match)
		if err != nil {
			return err
		}
		sd.re = re
	}
	return nil
}

// checkConflicts enforces one owner per host: no service name may equal a
// <sub>.<name> host of another service.
func checkConflicts(services map[string]*Service) error {
	for name, s := range services {
		for sub := range s.Subdomains {
			host := sub + "." + name
			if _, taken := services[host]; taken {
				return fmt.Errorf("%s.localhost is claimed twice: by service %s and by subdomain %s of %s", host, host, sub, name)
			}
		}
	}
	return nil
}

// lookup finds the service for a request host. An exact name wins; otherwise
// the first label is tried as a subdomain of the rest.
func lookup(services map[string]*Service, host string) (name, sub string, svc *Service) {
	n, ok := nameFromHost(host)
	if !ok {
		return "", "", nil
	}
	if s := services[n]; s != nil {
		return n, "", s
	}
	first, rest, found := strings.Cut(n, ".")
	if !found {
		return "", "", nil
	}
	if s := services[rest]; s != nil && s.Subdomains[first] != nil {
		return rest, first, s
	}
	return "", "", nil
}

// nameFromHost turns "foo.bar.localhost:443" into "foo.bar".
func nameFromHost(host string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	name, ok := strings.CutSuffix(host, ".localhost")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// ServiceURL is the HTTPS URL for a service name or <sub>.<name>.
func ServiceURL(name string) string { return "https://" + name + ".localhost" }

// subPort returns the port of a fixed-port subdomain, or the main port.
func (s *Service) subPort(sub string) int {
	if sub == "" {
		return s.Port
	}
	if sd := s.Subdomains[sub]; sd != nil {
		return sd.Port
	}
	return 0
}

// sortedNames returns the keys of services in order.
func sortedNames(services map[string]*Service) []string {
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Placeholders returns the {name} tokens in a command, in order of first use.
// "{{" and "}}" are literal braces.
func Placeholders(argv []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range argv {
		_ = expandArg(a, func(name string) (string, bool) {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
			return "", true
		})
	}
	return out
}

// ExpandCommand replaces {port} and {<sub>} with the assigned ports.
func ExpandCommand(argv []string, ports map[string]int) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = expandArg(a, func(name string) (string, bool) {
			key := name
			if name == "port" {
				key = ""
			}
			p, ok := ports[key]
			return strconv.Itoa(p), ok
		})
	}
	return out
}

// expandArg replaces each {label} via repl. Unknown placeholders, and text
// that isn't a valid label, stay as written.
func expandArg(s string, repl func(string) (string, bool)) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c == '{' || c == '}') && i+1 < len(s) && s[i+1] == c {
			b.WriteByte(c)
			i++
			continue
		}
		if c == '{' {
			if end := strings.IndexByte(s[i:], '}'); end > 0 {
				name := s[i+1 : i+end]
				if ValidLabel(name) {
					if v, ok := repl(name); ok {
						b.WriteString(v)
						i += end
						continue
					}
				}
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// expandHostHeader fills {port} in a host_header value.
func expandHostHeader(h string, port int) string {
	return strings.ReplaceAll(h, "{port}", strconv.Itoa(port))
}
