package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wizz666/nestward/internal/alert"
	"github.com/wizz666/nestward/internal/config"
	"github.com/wizz666/nestward/internal/i18n"
	"github.com/wizz666/nestward/internal/notify"
	"github.com/wizz666/nestward/internal/pki"
	"github.com/wizz666/nestward/internal/secrets"
	"github.com/wizz666/nestward/internal/server"
	"github.com/wizz666/nestward/internal/store"
	"github.com/wizz666/nestward/internal/watchdog"
)

// localHost is the address the monitor uses for web pages on this
// machine: the host.address setting, or a sensible default.
func localHost(s *config.Settings) func(context.Context) string {
	return func(ctx context.Context) string {
		if v, err := s.Get(ctx, config.HostAddress); err == nil && v != "" {
			return v
		}
		if _, err := os.Stat("/.dockerenv"); err == nil {
			// Set with --add-host=host.docker.internal:host-gateway. When it
			// is missing, the container's default gateway is the machine.
			if _, err := net.DefaultResolver.LookupHost(ctx, "host.docker.internal"); err == nil {
				return "host.docker.internal"
			}
			if gw := defaultGateway("/proc/net/route"); gw != "" {
				return gw
			}
			return "host.docker.internal"
		}
		return "127.0.0.1"
	}
}

// defaultGateway reads the default route's gateway from a Linux route
// table (/proc/net/route), or "" when there is none.
func defaultGateway(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		var gw uint32
		if _, err := fmt.Sscanf(f[2], "%08X", &gw); err != nil || gw == 0 {
			continue
		}
		// The kernel prints the address in host (little-endian) order.
		return net.IPv4(byte(gw), byte(gw>>8), byte(gw>>16), byte(gw>>24)).String()
	}
	return ""
}

func openSettings(data string) (*config.Settings, func(), error) {
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, nil, err
	}
	st, err := store.Open(filepath.Join(data, "nestward.db"))
	if err != nil {
		return nil, nil, err
	}
	box, err := secrets.OpenBox(filepath.Join(data, "secret.key"))
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return &config.Settings{Store: st, Box: box}, func() { st.Close() }, nil
}

func configCmd(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	data := fs.String("data", env("NESTWARD_DATA", "data"), "Nestward's data folder")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: nestward config [--data DIR] list | get NAME | set NAME VALUE\n\nSettings:\n")
		for _, k := range config.Keys {
			fmt.Fprintf(fs.Output(), "  %-14s %s\n", k.Name, k.Help)
		}
	}
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		rest = []string{"list"}
	}
	s, done, err := openSettings(*data)
	if err != nil {
		return err
	}
	defer done()
	switch rest[0] {
	case "list":
		list, err := s.List(ctx)
		if err != nil {
			return err
		}
		for _, e := range list {
			mark := " "
			if e.IsSet {
				mark = "*"
			}
			fmt.Fprintf(w, "%s %-14s %s\n", mark, e.Name, e.Value)
		}
		fmt.Fprintln(w, "\n* = set by you; the rest are defaults. Secret values are never shown.")
		return nil
	case "get":
		if len(rest) != 2 {
			return errors.New("usage: nestward config get NAME")
		}
		for _, k := range config.Keys {
			if k.Name == rest[1] && k.Secret {
				return fmt.Errorf("%s is secret and is not shown; set it again to change it", k.Name)
			}
		}
		v, err := s.Get(ctx, rest[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(w, v)
		return nil
	case "set":
		if len(rest) != 3 {
			return errors.New("usage: nestward config set NAME VALUE (an empty VALUE removes the setting, - reads it from standard input)")
		}
		value := rest[2]
		if value == "-" {
			// Keeps secrets off the command line and out of shell history.
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
			if err != nil {
				return err
			}
			value = strings.TrimSpace(string(b))
		}
		if err := s.Set(ctx, rest[1], value); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s saved.\n", rest[1])
		return nil
	}
	fs.Usage()
	return fmt.Errorf("unknown config action %q", rest[0])
}

// resetLogin removes the admin account and all sessions. Whoever can run
// this already controls the server, so it needs no password. The next
// start prints a new setup code.
func resetLogin(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("reset-login", flag.ExitOnError)
	data := fs.String("data", env("NESTWARD_DATA", "data"), "Nestward's data folder")
	fs.Parse(args)
	st, err := store.Open(filepath.Join(*data, "nestward.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.DeleteAdmin(ctx); err != nil {
		return err
	}
	fmt.Fprintln(w, "The web login was removed. Restart Nestward and use the new setup code from its log.")
	return nil
}

func testAlert(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("test-alert", flag.ExitOnError)
	data := fs.String("data", env("NESTWARD_DATA", "data"), "Nestward's data folder")
	fs.Parse(args)
	s, done, err := openSettings(*data)
	if err != nil {
		return err
	}
	defer done()
	bundle, err := i18n.Load()
	if err != nil {
		return err
	}
	a := &alert.Alerter{Settings: s, I18n: bundle}
	if err := a.SendTest(ctx); err != nil {
		return err
	}
	fmt.Fprintln(w, "Test alert sent. Check the phone.")
	return nil
}

func runWatchdog(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watchdog", flag.ExitOnError)
	listen := fs.String("listen", env("NESTWARD_WATCHDOG_LISTEN", ":8471"), "address to listen on")
	name := fs.String("name", env("NESTWARD_WATCHDOG_NAME", "Home"), "name of the home in the alert")
	timeout := fs.Duration("timeout", 3*time.Minute, "silence before the alert")
	lang := fs.String("lang", env("NESTWARD_LANG", "en"), "alert language: en or sv")
	fs.Parse(args)
	// Nestward sends a heartbeat every minute; a shorter wait would raise
	// the alarm between two normal heartbeats.
	if *timeout < 2*time.Minute {
		return errors.New("watchdog: --timeout must be at least 2m (the heartbeat comes once a minute)")
	}
	token := os.Getenv("NESTWARD_WATCHDOG_TOKEN")
	if len(token) < 16 || strings.ContainsAny(token, "/?# ") {
		return errors.New("watchdog: set NESTWARD_WATCHDOG_TOKEN to a random string of at least 16 letters and digits")
	}
	topic := os.Getenv("NTFY_TOPIC")
	if topic == "" {
		return errors.New("watchdog: set NTFY_TOPIC (and NTFY_URL, NTFY_TOKEN if needed)")
	}
	bundle, err := i18n.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	wd := &watchdog.Watchdog{
		Name:    *name,
		Token:   token,
		Timeout: *timeout,
		Sender:  notify.Ntfy{URL: env("NTFY_URL", "https://ntfy.sh"), Topic: topic, Token: os.Getenv("NTFY_TOKEN")},
		T:       bundle.For(*lang),
		Log:     log,
	}
	go wd.Run(ctx)
	hs := &http.Server{Addr: *listen, Handler: wd.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(c)
	}()
	log.Info("watchdog started", "listen", *listen, "timeout", timeout.String(), "ping_path", "/ping/<token>")
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// agentCode makes an add-machine code on the core's own machine, for
// people who would rather use a shell than the web page. The running
// core accepts it because codes live in the shared database.
func agentCode(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("agent-code", flag.ExitOnError)
	data := fs.String("data", env("NESTWARD_DATA", "data"), "Nestward's data folder")
	core := fs.String("core", "", "address the new machine uses to reach this core, e.g. 192.168.1.10 or https://192.168.1.10:8472")
	fs.Parse(args)
	if *core == "" {
		return errors.New("agent-code: --core is required (the address the other machine reaches this core at)")
	}
	addr := *core
	if !strings.HasPrefix(addr, "https://") {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			addr = net.JoinHostPort(addr, "8472")
		}
		addr = "https://" + addr
	}
	ca, err := pki.EnsureCA(filepath.Join(*data, "pki"))
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(*data, "nestward.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	code, exp, err := server.NewAgentCode(ctx, st)
	if err != nil {
		return err
	}
	program, docker := server.AgentCommands(addr, code, pki.Fingerprint(ca.Cert))
	fmt.Fprintf(w, "Code works once, until %s.\n\nWith Docker:\n  %s\n\nWith the nestward program:\n  %s\n",
		exp.Local().Format("15:04"), docker, program)
	return nil
}
