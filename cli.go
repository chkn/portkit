package portkit

import (
	"bufio"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/smallstep/truststore"
	"github.com/spf13/cobra"
)

// The portkit commands live in Caddy as `caddy portkit …`; the `portkit`
// wrapper on the PATH runs them with portkit's copy of Caddy.

func registerCommands() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "portkit",
		Usage: "<command> [args]",
		Short: "Give local dev servers HTTPS URLs at <name>.localhost",
		Long: `portkit proxies https://<name>.localhost to a fixed port, to a command
it starts on demand, or to a command already running in your terminal.`,
		CobraFunc: func(cmd *cobra.Command) {
			cmd.AddCommand(
				registerCmd(), runCmd(), adoptCmd(), lsCmd(), unregisterCmd(),
				stopCmd(), logsCmd(), statusCmd(), trustCmd(), upgradeConfigCmd(),
			)
		},
	})
}

// cli wraps a command so errors print as "portkit: …" with exit status 1.
func cli(f func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) {
	return func(cmd *cobra.Command, args []string) {
		if err := f(cmd, args); err != nil {
			fmt.Fprintln(os.Stderr, "portkit:", err)
			os.Exit(1)
		}
	}
}

// admin returns a client for the admin socket. Connecting starts Caddy if
// it is asleep.
func admin() *adminClient { return newSocketClient(AdminSocket()) }

// call is admin().do with a friendlier error when portkit isn't reachable.
func call(method, path string, in, out any) error {
	err := admin().do(method, path, in, out)
	var ae *adminError
	if err != nil && !errors.As(err, &ae) {
		return fmt.Errorf("can't reach portkit at %s: %w (is it installed? see `portkit status`)", AdminSocket(), err)
	}
	return err
}

// isNotFound reports whether a config GET failed because the path is missing.
func isNotFound(err error) bool {
	var ae *adminError
	if !errors.As(err, &ae) {
		return false
	}
	m := ae.Message
	return ae.Status == http.StatusNotFound || strings.Contains(m, "unknown object key") ||
		strings.Contains(m, "invalid traversal path") || strings.Contains(m, "not found")
}

// getService fetches one record, or nil if there is none.
func getService(name string) (*Service, error) {
	var s *Service
	err := call(http.MethodGet, servicePath(name), nil, &s)
	if isNotFound(err) {
		return nil, nil
	}
	return s, err
}

// svcFlags are the flags shared by register, run and adopt.
type svcFlags struct {
	labels      []string
	subs        []string
	idleTimeout string
	hostHeader  string
	env         []string
	cwd         string
	listenPort  int
}

func (f *svcFlags) addLabelSub(cmd *cobra.Command) {
	cmd.Flags().StringArrayVar(&f.labels, "label", nil, "add a label `key=value` (repeatable)")
	cmd.Flags().StringArrayVar(&f.subs, "sub", nil, "serve a subdomain: `NAME=PORT` or NAME=/regex/ (repeatable)")
}

func (f *svcFlags) addProcess(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.idleTimeout, "idle-timeout", "", "stop after this long without traffic: `30m` or never")
	cmd.Flags().StringVar(&f.hostHeader, "host-header", "", "Host header to send upstream, e.g. `localhost:{port}`")
	cmd.Flags().StringArrayVar(&f.env, "env", nil, "set a variable `KEY=VALUE` for the command (repeatable)")
	cmd.Flags().StringVar(&f.cwd, "cwd", "", "working `directory` (default: the current one)")
	cmd.Flags().IntVar(&f.listenPort, "listen-port", 0, "the `port` the command listens on; skips detection")
}

func parseKV(list []string, what string) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%s %q must be KEY=VALUE", what, kv)
		}
		m[k] = v
	}
	return m, nil
}

// parseSubs reads --sub NAME=PORT and NAME=/regex/.
func parseSubs(list []string, allowRegex bool) (map[string]*Subdomain, error) {
	subs := map[string]*Subdomain{}
	for _, item := range list {
		name, spec, ok := strings.Cut(item, "=")
		if !ok || !ValidLabel(name) {
			return nil, fmt.Errorf("--sub %q must be NAME=PORT or NAME=/regex/, with NAME a single label", item)
		}
		if strings.HasPrefix(spec, "/") && strings.HasSuffix(spec, "/") && len(spec) >= 2 {
			if !allowRegex {
				return nil, fmt.Errorf("--sub %s: adopt takes only fixed ports, since portkit never sees the server's output", name)
			}
			subs[name] = &Subdomain{Match: spec[1 : len(spec)-1]}
			continue
		}
		port, err := strconv.Atoi(spec)
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("--sub %s: %q is not a port or /regex/", name, spec)
		}
		subs[name] = &Subdomain{Port: port}
	}
	return subs, nil
}

// buildProcessService builds an on-demand or foreground record from the flags
// and the command, declaring a subdomain for each {placeholder}.
func (f *svcFlags) buildProcessService(mode string, command []string) (*Service, error) {
	s := &Service{Mode: mode, Command: command, IdleTimeout: f.idleTimeout, HostHeader: f.hostHeader, ListenPort: f.listenPort}
	var err error
	if s.Labels, err = parseKV(f.labels, "--label"); err != nil {
		return nil, err
	}
	subs, err := parseSubs(f.subs, true)
	if err != nil {
		return nil, err
	}
	for _, ph := range Placeholders(command) {
		if ph == "port" {
			continue
		}
		if _, dup := subs[ph]; dup {
			return nil, fmt.Errorf("subdomain %s is declared by both {%s} and --sub", ph, ph)
		}
		subs[ph] = &Subdomain{Assign: true}
	}
	if len(subs) > 0 {
		s.Subdomains = subs
	}
	cwd := f.cwd
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	if s.Cwd, err = filepath.Abs(cwd); err != nil {
		return nil, err
	}
	env, err := parseKV(f.env, "--env")
	if err != nil {
		return nil, err
	}
	if env == nil {
		env = map[string]string{}
	}
	// launchd and systemd give Caddy a minimal PATH, so save the caller's.
	if _, ok := env["PATH"]; !ok {
		env["PATH"] = os.Getenv("PATH")
	}
	s.Env = env
	return s, nil
}

func registerCmd() *cobra.Command {
	var f svcFlags
	cmd := &cobra.Command{
		Use:   "register NAME (PORT | -- COMMAND...)",
		Short: "Register NAME.localhost for a fixed port or an on-demand command",
		Long: `Register NAME.localhost.

  portkit register foo 3000
      Proxy https://foo.localhost to localhost:3000.

  portkit register foo -- npm run dev -- --port {port}
      Start the command on a free port whenever foo.localhost is visited.
      {port} and $PORT hold the assigned port; {api} declares a subdomain
      api.foo.localhost with a port of its own.`,
		Run: cli(func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			var name string
			var s *Service
			switch {
			case dash == 1 && len(args) > 1:
				name = args[0]
				var err error
				if s, err = f.buildProcessService(ModeOnDemand, args[1:]); err != nil {
					return err
				}
			case dash < 0 && len(args) == 2:
				name = args[0]
				port, err := strconv.Atoi(args[1])
				if err != nil || port <= 0 || port > 65535 {
					return fmt.Errorf("%q is not a port; to run a command, put it after --", args[1])
				}
				if f.idleTimeout != "" || len(f.env) > 0 || f.listenPort != 0 || f.cwd != "" {
					return errors.New("--idle-timeout, --env, --cwd and --listen-port apply only to commands")
				}
				s = &Service{Mode: ModePort, Port: port, HostHeader: f.hostHeader}
				if s.Labels, err = parseKV(f.labels, "--label"); err != nil {
					return err
				}
				subs, err := parseSubs(f.subs, false)
				if err != nil {
					return err
				}
				if len(subs) > 0 {
					s.Subdomains = subs
				}
			default:
				return errors.New("usage: portkit register NAME PORT, or portkit register NAME -- COMMAND...")
			}
			if err := s.provision(name); err != nil {
				return err
			}
			if s.Mode == ModeOnDemand {
				warnPrivacyFolder(s.Cwd)
			}
			if err := call(http.MethodPost, servicePath(name), s, nil); err != nil {
				return err
			}
			if s.Mode == ModePort {
				fmt.Printf("%s → localhost:%d\n", ServiceURL(name), s.Port)
			} else {
				fmt.Printf("%s starts on first visit: %s\n  in %s\n", ServiceURL(name), strings.Join(s.Command, " "), s.Cwd)
			}
			for _, sub := range sortedKeys(s.Subdomains) {
				fmt.Printf("%s\n", ServiceURL(sub+"."+name))
			}
			return nil
		}),
	}
	f.addLabelSub(cmd)
	f.addProcess(cmd)
	return cmd
}

// warnPrivacyFolder warns that macOS privacy protection applies to
// processes Caddy starts in ~/Documents, ~/Desktop or ~/Downloads.
func warnPrivacyFolder(dir string) {
	if runtime.GOOS != "darwin" {
		return
	}
	home, _ := os.UserHomeDir()
	for _, d := range []string{"Documents", "Desktop", "Downloads"} {
		p := filepath.Join(home, d)
		if dir == p || strings.HasPrefix(dir, p+"/") {
			fmt.Fprintf(os.Stderr, "portkit: warning: %s is in ~/%s. macOS will ask whether \"caddy\" may access it, or block it.\n", dir, d)
			fmt.Fprintf(os.Stderr, "         Moving the project out of ~/%s avoids this.\n", d)
			return
		}
	}
}

func runCmd() *cobra.Command {
	var f svcFlags
	var replace bool
	cmd := &cobra.Command{
		Use:   "run NAME -- COMMAND...",
		Short: "Run a command in this terminal and route NAME.localhost to it until it exits",
		Run: cli(func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 1 || len(args) < 2 {
				return errors.New("usage: portkit run NAME -- COMMAND...")
			}
			name := args[0]
			if err := ValidateName(name); err != nil {
				return err
			}
			s, err := f.buildProcessService(ModeForeground, args[1:])
			if err != nil {
				return err
			}
			code, err := runForeground(name, s, replace)
			if err != nil {
				return err
			}
			os.Exit(code)
			return nil
		}),
	}
	f.addLabelSub(cmd)
	f.addProcess(cmd)
	cmd.Flags().BoolVar(&replace, "replace", false, "take over NAME if it is registered in another mode, restoring it on exit")
	return cmd
}

func adoptCmd() *cobra.Command {
	var f svcFlags
	var pid int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "adopt NAME PORT",
		Short: "Route NAME.localhost to a running server for as long as the calling process lives",
		Long: `Route NAME.localhost to a server that is already running, for as long as
the process that ran this command lives. Meant to be run by the server
itself at startup:

  portkit adopt myapp.evalution 4100 --label kind=evalution

If a service already routes to PORT, its labels and subdomains are merged
into that service instead, and its URL is printed.`,
		Run: cli(func(cmd *cobra.Command, args []string) error {
			if len(args) != 2 {
				return errors.New("usage: portkit adopt NAME PORT")
			}
			name := args[0]
			if err := ValidateName(name); err != nil {
				return err
			}
			port, err := strconv.Atoi(args[1])
			if err != nil || port <= 0 || port > 65535 {
				return fmt.Errorf("%q is not a port", args[1])
			}
			if pid == 0 {
				pid = os.Getppid()
			}
			if uid, err := ProcessUID(pid); err != nil || uid != os.Getuid() {
				return fmt.Errorf("process %d isn't a running process of this user", pid)
			}
			owner, err := OwnerOf(pid)
			if err != nil {
				return fmt.Errorf("process %d: %w", pid, err)
			}
			req := AdoptRequest{Name: name, Port: port, Owner: *owner}
			if req.Labels, err = parseKV(f.labels, "--label"); err != nil {
				return err
			}
			subs, err := parseSubs(f.subs, false)
			if err != nil {
				return err
			}
			for sub, sd := range subs {
				if req.Subdomains == nil {
					req.Subdomains = map[string]int{}
				}
				req.Subdomains[sub] = sd.Port
			}
			if req.Cwd, err = os.Getwd(); err != nil {
				return err
			}
			var resp AdoptResponse
			if err := call(http.MethodPost, "/portkit/adopt", req, &resp); err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"name": resp.Name, "url": resp.URL, "action": resp.Action, "record": resp.Record})
			}
			fmt.Println(resp.URL)
			return nil
		}),
	}
	f.addLabelSub(cmd)
	cmd.Flags().IntVar(&pid, "pid", 0, "the owner `process` (default: the parent process)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the final record as JSON")
	return cmd
}

func lsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List services",
		Args:    cobra.NoArgs,
		Run: cli(func(cmd *cobra.Command, args []string) error {
			var services map[string]*Service
			if err := call(http.MethodGet, "/config/apps/portkit/services", nil, &services); err != nil && !isNotFound(err) {
				return err
			}
			state := map[string]ServiceState{}
			if err := call(http.MethodGet, "/portkit/state", nil, &state); err != nil {
				return err
			}
			if asJSON {
				out := map[string]any{}
				for name, s := range services {
					out[name] = map[string]any{"record": s, "state": state[name]}
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			if len(services) == 0 {
				fmt.Println("No services. Register one with `portkit register NAME PORT`.")
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "URL\tMODE\tSTATE\tUPSTREAM\tDIRECTORY")
			for _, name := range sortedNames(services) {
				s, st := services[name], state[name]
				upstream := st.Addr
				if upstream == "" && s.Port != 0 {
					upstream = "localhost:" + strconv.Itoa(s.Port)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ServiceURL(name), modeLabel(s.Mode), st.State, dash(upstream), dash(tildePath(s.Cwd)))
				for _, sub := range sortedKeys(s.Subdomains) {
					up := st.Subdomains[sub]
					if up == "" && s.Subdomains[sub].Port != 0 {
						up = "localhost:" + strconv.Itoa(s.Subdomains[sub].Port)
					}
					fmt.Fprintf(tw, "  %s\t\t\t%s\t\n", ServiceURL(sub+"."+name), dash(up))
				}
			}
			return tw.Flush()
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print records and state as JSON")
	return cmd
}

func modeLabel(mode string) string {
	if mode == ModeOnDemand {
		return "on demand"
	}
	return mode
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func tildePath(p string) string {
	home, _ := os.UserHomeDir()
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func unregisterCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "unregister NAME",
		Aliases: []string{"rm"},
		Short:   "Remove a service and stop its process",
		Args:    cobra.ExactArgs(1),
		Run: cli(func(cmd *cobra.Command, args []string) error {
			err := call(http.MethodDelete, servicePath(args[0]), nil, nil)
			if isNotFound(err) {
				return fmt.Errorf("no service named %s", args[0])
			}
			if err == nil {
				fmt.Printf("Removed %s\n", ServiceURL(args[0]))
			}
			return err
		}),
	}
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop NAME",
		Short: "Stop a running on-demand process; the next visit starts it again",
		Args:  cobra.ExactArgs(1),
		Run: cli(func(cmd *cobra.Command, args []string) error {
			var resp struct {
				Stopped bool `json:"stopped"`
				Reset   bool `json:"reset"`
			}
			if err := call(http.MethodPost, "/portkit/stop", map[string]string{"name": args[0]}, &resp); err != nil {
				return err
			}
			switch {
			case resp.Stopped:
				fmt.Printf("Stopped %s\n", args[0])
			case resp.Reset:
				fmt.Printf("%s had stopped after crashing; the next visit starts it again\n", args[0])
			default:
				fmt.Printf("%s wasn't running\n", args[0])
			}
			return nil
		}),
	}
}

func logsCmd() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "logs NAME",
		Short: "Show a service's output",
		Args:  cobra.ExactArgs(1),
		Run: cli(func(cmd *cobra.Command, args []string) error {
			name := args[0]
			path := filepath.Join(LogDir(), name+".log")
			f, err := os.Open(path)
			if errors.Is(err, os.ErrNotExist) {
				if s, _ := getService(name); s != nil && s.Mode != ModeOnDemand {
					return fmt.Errorf("%s is a %s service; portkit doesn't run it, so it has no log here", name, s.Mode)
				}
				return fmt.Errorf("no log for %s yet (%s)", name, path)
			}
			if err != nil {
				return err
			}
			defer f.Close()
			if err := printTail(f, lines); err != nil {
				return err
			}
			if !follow {
				return nil
			}
			return followFile(f, path)
		}),
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new output")
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "number of lines to show; 0 for all")
	return cmd
}

// printTail prints the last n lines of f (all if n is 0) and leaves f at
// its end.
func printTail(f *os.File, n int) error {
	if n == 0 {
		_, err := io.Copy(os.Stdout, f)
		return err
	}
	var ring []string
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			ring = append(ring, line)
			if len(ring) > n {
				ring = ring[1:]
			}
		}
		if err != nil {
			break
		}
	}
	for _, l := range ring {
		os.Stdout.WriteString(l)
	}
	return nil
}

// followFile prints what is appended to path, reopening it when it rotates.
func followFile(f *os.File, path string) error {
	for {
		if _, err := io.Copy(os.Stdout, f); err != nil {
			return err
		}
		time.Sleep(250 * time.Millisecond)
		cur, err1 := f.Stat()
		now, err2 := os.Stat(path)
		if err1 == nil && err2 == nil && !os.SameFile(cur, now) {
			nf, err := os.Open(path)
			if err == nil {
				_, _ = io.Copy(os.Stdout, f)
				f.Close()
				f = nf
			}
		}
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether Caddy is running, the admin socket and the CA certificate",
		Args:  cobra.NoArgs,
		Run: cli(func(cmd *cobra.Command, args []string) error {
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			defer tw.Flush()
			var info Info
			err := admin().do(http.MethodGet, "/portkit/info", nil, &info)
			if err != nil {
				fmt.Fprintf(tw, "Caddy:\tnot reachable (%v)\n", err)
			} else {
				fmt.Fprintf(tw, "Caddy:\trunning, pid %d, up %s\n", info.PID, time.Since(info.Started).Round(time.Second))
				fmt.Fprintf(tw, "Version:\t%s\n", info.Version)
				fmt.Fprintf(tw, "Activity:\t%d connections, %d processes; exits after %s idle\n", info.Connections, info.Children, info.IdleExit)
			}
			fmt.Fprintf(tw, "Admin socket:\t%s\n", AdminSocket())
			ca := CACertPath()
			if err == nil && info.CACert != "" {
				ca = info.CACert
			}
			trust := "not created yet"
			if cert, err := readCert(ca); err == nil {
				trust = "not trusted; run `portkit trust`"
				if _, err := cert.Verify(x509.VerifyOptions{}); err == nil {
					trust = "trusted"
				}
			}
			fmt.Fprintf(tw, "CA certificate:\t%s (%s)\n", ca, trust)
			fmt.Fprintf(tw, "Home:\t%s\n", Home())
			return nil
		}),
	}
}

func readCert(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM data")
	}
	return x509.ParseCertificate(block.Bytes)
}

func trustCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "trust",
		Short: "Add the portkit CA to the system trust store",
		Args:  cobra.NoArgs,
		Run: cli(func(cmd *cobra.Command, args []string) error {
			// Asking the admin API starts Caddy, which creates the CA.
			var ca struct {
				RootCert string `json:"root_certificate"`
			}
			if err := call(http.MethodGet, "/pki/ca/local", nil, &ca); err != nil {
				return err
			}
			block, _ := pem.Decode([]byte(ca.RootCert))
			if block == nil {
				return errors.New("admin API returned no root certificate")
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return err
			}
			if _, err := cert.Verify(x509.VerifyOptions{}); err == nil {
				fmt.Println("The portkit CA is already trusted.")
			} else {
				fmt.Println("Adding the portkit CA to the system trust store (this asks for your password)...")
				if err := truststore.Install(cert, truststore.WithFirefox()); err != nil {
					return fmt.Errorf("installing the CA: %w", err)
				}
				fmt.Println("Trusted.")
			}
			if _, err := exec.LookPath("certutil"); err != nil {
				fmt.Println("For Firefox, install certutil (brew install nss, or libnss3-tools) and run `portkit trust` again,")
				fmt.Println("or import " + CACertPath() + " in Firefox's certificate settings.")
			}
			return nil
		}),
	}
}

// upgradeConfigCmd carries the service records from autosave.json over to a
// new bootstrap.json, so an upgrade can change the rest of the config. The
// install script runs it while Caddy is stopped.
func upgradeConfigCmd() *cobra.Command {
	var bootstrap, autosave string
	cmd := &cobra.Command{
		Use:    "upgrade-config",
		Short:  "Rebuild autosave.json from bootstrap.json, keeping services",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run: cli(func(cmd *cobra.Command, args []string) error {
			n, err := upgradeConfig(bootstrap, autosave)
			if err != nil {
				return err
			}
			if n >= 0 {
				fmt.Printf("Kept %d services in %s\n", n, autosave)
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&bootstrap, "bootstrap", filepath.Join(Home(), "bootstrap.json"), "the new bootstrap config")
	cmd.Flags().StringVar(&autosave, "autosave", filepath.Join(Home(), "config", "caddy", "autosave.json"), "the saved config to rewrite")
	return cmd
}

// upgradeConfig rewrites autosave as bootstrap plus autosave's services. It
// returns -1 if there is no autosave yet.
func upgradeConfig(bootstrapPath, autosavePath string) (int, error) {
	saved, err := os.ReadFile(autosavePath)
	if errors.Is(err, os.ErrNotExist) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	fresh, err := os.ReadFile(bootstrapPath)
	if err != nil {
		return 0, err
	}
	var old struct {
		Apps struct {
			Portkit struct {
				Services map[string]json.RawMessage `json:"services"`
			} `json:"portkit"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(saved, &old); err != nil {
		return 0, fmt.Errorf("%s: %w", autosavePath, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(fresh, &cfg); err != nil {
		return 0, fmt.Errorf("%s: %w", bootstrapPath, err)
	}
	apps, _ := cfg["apps"].(map[string]any)
	pk, _ := apps["portkit"].(map[string]any)
	if pk == nil {
		return 0, fmt.Errorf("%s has no apps.portkit", bootstrapPath)
	}
	services := map[string]json.RawMessage{}
	for name, rec := range old.Apps.Portkit.Services {
		services[name] = rec
	}
	pk["services"] = services
	out, err := json.Marshal(cfg)
	if err != nil {
		return 0, err
	}
	tmp := autosavePath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return 0, err
	}
	return len(services), os.Rename(tmp, autosavePath)
}
