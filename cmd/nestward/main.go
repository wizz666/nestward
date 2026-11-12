// Command nestward is the Nestward core and agent in one program.
//
//	nestward serve    start the core with its web interface
//	nestward agent    report this machine's hardware to a core
//	nestward report   print this machine's hardware report
//	nestward discover print the services running on this machine
//	nestward config   show or change settings (alerts, heartbeat)
//	nestward test-alert send a test alert to the phone
//	nestward watchdog run outside the home and alert when it goes quiet
//	nestward version  print the version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // alert times in local time even in minimal containers

	"github.com/wizz666/nestward/internal/agent"
	"github.com/wizz666/nestward/internal/alert"
	"github.com/wizz666/nestward/internal/analysis"
	"github.com/wizz666/nestward/internal/auth"
	"github.com/wizz666/nestward/internal/catalog"
	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/discovery"
	"github.com/wizz666/nestward/internal/exposure"
	"github.com/wizz666/nestward/internal/hardware"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/monitor"
	"github.com/wizz666/nestward/internal/pki"
	"github.com/wizz666/nestward/internal/proxmox"
	"github.com/wizz666/nestward/internal/publish"
	"github.com/wizz666/nestward/internal/secrets"
	"github.com/wizz666/nestward/internal/server"
	"github.com/wizz666/nestward/internal/store"
	"github.com/wizz666/nestward/internal/tailnet"
	"github.com/wizz666/nestward/internal/version"
	"github.com/wizz666/nestward/profiles"
	"github.com/wizz666/nestward/signatures"
)

const usage = `Nestward – an ops layer for the home server you already have.

Usage:
  nestward serve   [--addr 127.0.0.1:8470] [--data ./data]
  nestward agent   [--data ./agent-data] [--core https://core:8472 --code CODE --fingerprint sha256:…] [--once]
  nestward report  [--lang en|sv] [--json]
  nestward discover [--json] [--from FILE]
  nestward config  [--data ./data] list | get NAME | set NAME VALUE
  nestward test-alert [--data ./data]
  nestward watchdog [--listen :8471] [--name Home] [--timeout 3m] [--lang en|sv]
  nestward agent-code --core HOST [--data ./data]  make an add-machine code on the core's own machine
  nestward reset-login [--data ./data]   remove the web login (lost password or phone)
  nestward version

Environment:
  NESTWARD_ADDR, NESTWARD_DATA   defaults for serve
  NESTWARD_HOST_ROOT             where the host's / is mounted when running in a container
  NESTWARD_AGENT_ADDR            the core's encrypted agent port (default :8472)
  DOCKER_HOST                    Docker to look at (default unix:///var/run/docker.sock)
  NESTWARD_DISCOVERY=off         do not look for services
  NESTWARD_WATCHDOG_TOKEN        secret part of the watchdog's ping address
  NTFY_URL, NTFY_TOPIC, NTFY_TOKEN  where the watchdog sends its alert
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "agent":
		err = runAgent(ctx, os.Args[2:])
	case "report":
		err = report(ctx, os.Args[2:], os.Stdout)
	case "discover":
		err = discover(ctx, os.Args[2:], os.Stdout)
	case "config":
		err = configCmd(ctx, os.Args[2:], os.Stdout)
	case "test-alert":
		err = testAlert(ctx, os.Args[2:], os.Stdout)
	case "watchdog":
		err = runWatchdog(ctx, os.Args[2:])
	case "agent-code":
		err = agentCode(ctx, os.Args[2:], os.Stdout)
	case "reset-login":
		err = resetLogin(ctx, os.Args[2:], os.Stdout)
	case "version", "--version", "-v":
		fmt.Println("nestward", version.Version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nestward:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func detector() func(context.Context) hardware.Report {
	d := hardware.NewDetector(os.Getenv("NESTWARD_HOST_ROOT"))
	return d.Detect
}

// discoverer returns the service finder, or nil when turned off.
func discoverer() (func(context.Context) discovery.Result, error) {
	if os.Getenv("NESTWARD_DISCOVERY") == "off" {
		return nil, nil
	}
	cat, err := discovery.LoadCatalog(signatures.FS)
	if err != nil {
		return nil, err
	}
	// On a Proxmox host its virtual machines and containers are services too.
	var pve *proxmox.Reader
	if os.Getenv("NESTWARD_PROXMOX") != "off" && proxmox.Available() {
		pve = &proxmox.Reader{}
	}
	return func(ctx context.Context) discovery.Result {
		r := discovery.Discover(ctx, cat, discovery.DockerLister)
		if pve != nil {
			gs, err := pve.Guests(ctx)
			discovery.AddGuests(&r, gs, cat, err)
		}
		return r
	}, nil
}

func discover(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON instead of text")
	from := fs.String("from", "", "read a saved Docker /containers/json answer instead of asking Docker")
	fs.Parse(args)
	cat, err := discovery.LoadCatalog(signatures.FS)
	if err != nil {
		return err
	}
	lister := discovery.DockerLister
	if *from != "" {
		lister = func() (discovery.Lister, error) { return discovery.FileLister(*from), nil }
	}
	r := discovery.Discover(ctx, cat, lister)
	if *from == "" && os.Getenv("NESTWARD_PROXMOX") != "off" && proxmox.Available() {
		pr := &proxmox.Reader{}
		gs, err := pr.Guests(ctx)
		if err == nil {
			pr.WaitLookups() // this command runs once: wait for the addresses
			gs, err = pr.Guests(ctx)
		}
		discovery.AddGuests(&r, gs, cat, err)
	}
	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	if !r.Available {
		return fmt.Errorf("could not list services: %s", r.Error)
	}
	for _, s := range r.Services {
		link := ""
		if s.Web != nil {
			host := s.Web.Host
			if host == "" {
				host = "<this machine>"
			}
			link = fmt.Sprintf("%s://%s:%d%s", s.Web.Scheme, host, s.Web.Port, s.Web.Path)
		}
		part := ""
		if s.PartOf != "" {
			part = "  (" + s.PartOf + ")"
		}
		fmt.Fprintf(w, "%-8s %-9s %-28s %-26s %s%s\n", s.Kind, s.State, s.Name, s.AppName, link, part)
	}
	st := r.Stats
	fmt.Fprintf(w, "\n%d containers, %d running. Recognised %d of %d public images (%d%%), %d built locally.\n",
		st.Total, st.Running, st.Recognized, st.Total-st.Own, st.Percent, st.Own)
	return nil
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", env("NESTWARD_ADDR", "127.0.0.1:8470"), "address to listen on")
	data := fs.String("data", env("NESTWARD_DATA", "data"), "folder for the database and keys")
	every := fs.Duration("check-interval", time.Minute, "time between monitoring rounds (two failed rounds raise an alert)")
	agentAddr := fs.String("agent-addr", env("NESTWARD_AGENT_ADDR", ":8472"), "encrypted port where agents report (empty = no agents)")
	fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	profs, err := catalog.Load(profiles.FS)
	if err != nil {
		return err
	}
	bundle, err := i18n.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*data, "nestward.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	// An older version kept a shared agent token; agents now use certificates.
	_ = os.Remove(filepath.Join(*data, "agent-token"))

	disc, err := discoverer()
	if err != nil {
		return err
	}
	box, err := secrets.OpenBox(filepath.Join(*data, "secret.key"))
	if err != nil {
		return err
	}
	settings := &config.Settings{Store: st, Box: box}
	alerter := &alert.Alerter{Settings: settings, I18n: bundle}
	heartbeat := &alert.Heartbeat{Settings: settings, Log: log}
	authm := &auth.Manager{Store: st, Box: box}
	// NESTWARD_CLOUDFLARE_API points at a fake API for development.
	pub := &publish.Engine{Settings: settings, Store: st, CloudflareBase: os.Getenv("NESTWARD_CLOUDFLARE_API")}
	// Each service published in Tailscale is its own node; their
	// identities live in the data folder so they survive restarts.
	tsRunner := &tailnet.Runner{Dir: filepath.Join(*data, "tailscale"), Log: log,
		APIKey: func(ctx context.Context) string {
			k, _ := settings.Get(ctx, config.TailscaleAPIKey)
			return k
		}}
	pub.Tailnet = tsRunner
	defer tsRunner.Close()
	go pub.Restore(ctx, log)
	srv := &server.Server{Store: st, Profiles: profs, I18n: bundle, Detect: detector(), Discover: disc,
		Log: log, Alerter: alerter, Heartbeat: heartbeat,
		Auth: authm, Settings: settings, Publish: pub}
	if need, err := authm.NeedsSetup(ctx); err != nil {
		return err
	} else if need {
		// Only someone who can read this log can create the account.
		code := authm.SetupCode()
		fmt.Fprintf(os.Stderr, "\n  First-time setup: open the web page and enter this code: %s\n  Första start: öppna webbsidan och skriv in den här koden: %s\n\n", code, code)
		log.Warn("waiting for first-time setup", "setup_code", code)
	}
	srv.Monitor = &monitor.Monitor{
		Store:     st,
		Machines:  srv.MonitorMachines,
		LocalHost: localHost(settings),
		OnEvents:  alerter.HandleEvents,
		Log:       log,
	}
	if *every < 5*time.Second {
		return errors.New("serve: --check-interval must be at least 5s")
	}
	go srv.Monitor.Run(ctx, *every)
	// What the home shows to the internet: soon after start, then every 6 hours.
	srv.Exposure = &exposure.Scanner{Settings: settings, Store: st, Prober: &exposure.Prober{},
		Notifier: alerter, Log: log, CloudflareBase: os.Getenv("NESTWARD_CLOUDFLARE_API")}
	go srv.Exposure.Run(ctx, 2*time.Minute, 6*time.Hour)
	go heartbeat.Run(ctx, time.Minute)

	// The encrypted agent port, with Nestward's own certificate authority.
	var agentSrv *http.Server
	if *agentAddr != "" {
		ca, err := pki.EnsureCA(filepath.Join(*data, "pki"))
		if err != nil {
			return err
		}
		cert, err := ca.ServerCert()
		if err != nil {
			return err
		}
		_, portStr, err := net.SplitHostPort(*agentAddr)
		if err != nil {
			return fmt.Errorf("--agent-addr: %w", err)
		}
		port, _ := strconv.Atoi(portStr)
		srv.Agents = &server.AgentGate{CA: ca, Port: port, Store: srv.Store}
		agentSrv = &http.Server{Addr: *agentAddr, Handler: srv.AgentHandler(), TLSConfig: ca.ServerConfig(cert),
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
		go func() {
			if err := agentSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("agent port stopped", "err", err)
			}
		}()
		log.Info("agent port open (TLS)", "address", *agentAddr, "fingerprint", pki.Fingerprint(ca.Cert))
	}

	hs := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
		if agentSrv != nil {
			agentSrv.Shutdown(shutdown)
		}
	}()
	log.Info("nestward started", "version", version.Version, "address", "http://"+*addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runAgent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	data := fs.String("data", env("NESTWARD_DATA", "agent-data"), "folder for the agent's key and certificate")
	core := fs.String("core", "", "the core's agent port, e.g. https://192.168.1.10:8472 (only when adding this machine)")
	code := fs.String("code", "", "one-time code from \"Add machine\" in Nestward (only when adding this machine)")
	fingerprint := fs.String("fingerprint", "", "the core's fingerprint from the same command")
	interval := fs.Duration("interval", 30*time.Second, "time between reports (the core calls a machine silent after 2 minutes)")
	once := fs.Bool("once", false, "send one report and exit")
	fs.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// After the core has removed this machine the agent stays quiet: it
	// neither sends nor exits (exiting would make Docker restart it over
	// and over). A new add-machine command brings it back.
	quiet := func() error {
		log.Error(agent.ErrRemoved.Error())
		if *once {
			return agent.ErrRemoved
		}
		<-ctx.Done()
		return nil
	}

	detect := detector()
	removed := agent.Removed(*data)
	if _, _, err := agent.Load(*data); errors.Is(err, agent.ErrNotEnrolled) || removed {
		if *core == "" || *code == "" || *fingerprint == "" {
			if removed {
				return quiet()
			}
			return errors.New("agent: this machine is not added yet; use the command from \"Add machine\" in Nestward")
		}
		hw := detect(ctx)
		if err := agent.Enroll(ctx, *data, *core, *code, *fingerprint, hw.MachineID, hw.Hostname); err != nil {
			if removed {
				// Most likely the old, used code from the first command.
				log.Warn("could not add this machine again", "err", err)
				return quiet()
			}
			return err
		}
		log.Info("this machine is now added to Nestward", "core", *core)
	}
	id, client, err := agent.Load(*data)
	if err != nil {
		return err
	}
	disc, err := discoverer()
	if err != nil {
		return err
	}
	collect := func(ctx context.Context) agent.Payload {
		p := agent.Payload{Hardware: detect(ctx)}
		if disc != nil {
			d := disc(ctx)
			p.Discovery = &d
		}
		return p
	}
	a := &agent.Agent{CoreURL: id.Core, Client: client, Interval: *interval, Collect: collect, Log: log}
	if *once {
		err = a.Send(ctx)
	} else {
		log.Info("agent running", "core", id.Core, "interval", interval.String())
		err = a.Run(ctx)
	}
	if errors.Is(err, agent.ErrRemoved) {
		if merr := agent.MarkRemoved(*data); merr != nil {
			log.Warn("could not remember the removal", "err", merr)
		}
		return quiet()
	}
	return err
}

func report(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	bundle, err := i18n.Load()
	if err != nil {
		return err
	}
	lang := fs.String("lang", bundle.Match(env("LANG", "en")), "language: en or sv")
	asJSON := fs.Bool("json", false, "print JSON instead of text")
	fs.Parse(args)
	profs, err := catalog.Load(profiles.FS)
	if err != nil {
		return err
	}
	t := bundle.For(*lang)
	r := analysis.Analyze(detector()(ctx), profs, t)
	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	return printText(w, r, t)
}

func printText(w io.Writer, r analysis.Report, t i18n.T) error {
	marks := map[analysis.Verdict]string{analysis.OK: "[+]", analysis.Slow: "[~]", analysis.No: "[-]", analysis.Unknown: "[?]"}
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	p("%s – %s\n\n", t.Get("app.name"), r.Machine.Title)
	p("%s\n", t.Get("ui.hardware_heading"))
	rows := [][2]string{
		{t.Get("ui.system"), r.Machine.System},
		{t.Get("ui.cpu"), r.Machine.CPU},
		{t.Get("ui.memory"), r.Machine.Memory},
		{t.Get("ui.graphics"), r.Machine.Graphics},
		{t.Get("ui.storage"), r.Machine.Storage},
		{t.Get("ui.virtualization"), r.Machine.Virtualization},
	}
	for _, row := range rows {
		p("  %-20s %s\n", row[0]+":", row[1])
	}
	p("\n%s\n", t.Get("ui.apps_heading"))
	for _, a := range r.Apps {
		p("  %s %-16s %s\n", marks[a.Verdict], a.Name, a.VerdictText)
		for _, rs := range a.Reasons {
			p("        - %s\n", rs.Text)
		}
		if a.Suggestion != "" {
			p("        %s: %s\n", t.Get("ui.suggestion"), a.Suggestion)
		}
	}
	p("\n%s\n", r.Summary)
	return nil
}
