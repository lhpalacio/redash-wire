package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/lhpalacio/redash-wire/internal/config"
	"github.com/lhpalacio/redash-wire/internal/mysqlwire"
	"github.com/lhpalacio/redash-wire/internal/proxy"
	"github.com/lhpalacio/redash-wire/internal/redash"
	"github.com/lhpalacio/redash-wire/internal/setup"
	"golang.org/x/term"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

const (
	formatText = "text"
	formatJSON = "json"
)

// JSON adds an RFC3339 timestamp to every event, which the human format omits.
func newLogger(w io.Writer, format string, debug bool) (*slog.Logger, error) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	opts := log.Options{
		Level:      log.Level(level),
		TimeFormat: time.Kitchen,
	}

	switch format {
	case formatText:
	case formatJSON:
		opts.Formatter = log.JSONFormatter
		opts.ReportTimestamp = true
		opts.TimeFormat = time.RFC3339
	default:
		return nil, fmt.Errorf("invalid -log-format %q: want %q or %q", format, formatText, formatJSON)
	}

	return slog.New(log.NewWithOptions(w, opts)), nil
}

func main() {
	// A leading non-flag argument selects a subcommand; anything else still serves.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(dispatch(os.Args[1], os.Args[2:]))
	}

	serve()
}

func serve() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	profile := flag.String("profile", "", "config profile to use (overrides default_profile in config)")
	debug := flag.Bool("debug", false, "enable debug logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	logFormat := flag.String("log-format", formatText, "log output format: text or json")
	exitOnStdinEOF := flag.Bool("exit-on-stdin-eof", false, "shut down when stdin reaches EOF, for use under a supervising parent process")
	flag.Parse()

	if *showVersion {
		fmt.Println("redash-wire", version)
		return
	}

	logger, err := newLogger(os.Stderr, *logFormat, *debug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	// A program is reading, so suppress the banner and the wizard.
	machineReadable := *logFormat == formatJSON

	configExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configExplicit = true
		}
	})

	resolved, err := config.Resolve(*configPath, configExplicit)
	if err != nil {
		logger.Error("resolving config path", "error", err)
		os.Exit(1)
	}

	if !resolved.Found {
		if configExplicit {
			logger.Error("config file not found", "path", shortenHome(resolved.Path))
			os.Exit(1)
		}

		if machineReadable {
			logger.Error("no config file found, and the setup wizard cannot run under -log-format=json",
				"path", shortenHome(resolved.Path))
			os.Exit(1)
		}

		if !term.IsTerminal(int(os.Stdin.Fd())) {
			logger.Error("no config file found: run interactively to set up, or create ~/.redash-wire/config.yaml manually")
			os.Exit(1)
		}

		title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render("redash-wire")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintf(os.Stderr, "  Welcome to %s! Let's set up your configuration.\n\n", title)

		result, err := setup.RunWizard(*profile)
		if err != nil {
			logger.Error("setup wizard", "error", err)
			os.Exit(1)
		}

		fc := config.NewFileConfig(result.ProfileName, result.RedashURL, result.APIKey, result.Username, result.Password, result.HasPostgres, result.HasMySQL)
		if err := config.WriteConfig(resolved.Path, fc); err != nil {
			logger.Error("writing config", "error", err)
			os.Exit(1)
		}

		// Load the profile the wizard just created, regardless of the -profile flag
		// (which may name a profile that does not exist yet on a fresh install).
		*profile = result.ProfileName

		dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		fmt.Fprintf(os.Stderr, "\n  %s %s\n\n",
			dim.Render("Config saved to"),
			lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Render(shortenHome(resolved.Path)))
	}

	cfg, err := config.Load(resolved.Path, *profile)
	if err != nil {
		logger.Error("loading config", "error", err)
		os.Exit(1)
	}

	warnDefaultCredentials(logger, cfg)

	// Auto-discovered cwd config is a secret-exfiltration risk: an untrusted
	// directory's config.yaml could point at an attacker URL and ${ENV} expand a
	// real API key. Make the source visible before any request goes out.
	if resolved.FromCwd {
		logger.Warn("using ./config.yaml from the current directory", "path", shortenHome(resolved.Path), "redash_url", cfg.RedashURL)
	} else {
		logger.Info("loaded config", "path", shortenHome(resolved.Path), "profile", cfg.Profile)
	}

	redashClient := redash.NewClient(
		cfg.RedashURL,
		cfg.APIKey,
		redash.WithPollInterval(cfg.GetPollInterval()),
		redash.WithPollTimeout(cfg.GetPollTimeout()),
	)

	session, err := redashClient.GetSession(context.Background())
	if err != nil {
		logger.Warn("fetching session info", "error", err)
	}

	sources, err := redashClient.ListDataSources(context.Background())
	if err != nil {
		logger.Error("fetching data sources", "error", err)
		os.Exit(1)
	}

	if len(sources) == 0 {
		logger.Error("no data sources found")
		os.Exit(1)
	}

	registry := redash.NewDataSourceRegistry(sources)

	if !machineReadable {
		printBanner(cfg, resolved.Path, sources, session)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// A nil channel blocks forever, so without the flag this case never fires.
	var stdinClosed chan struct{}
	if *exitOnStdinEOF {
		stdinClosed = make(chan struct{})
		go func() {
			// macOS reparents a child instead of killing it, so a force-quit
			// would otherwise leave an orphan holding the ports. The write end
			// of this pipe closes however the parent dies.
			_, _ = io.Copy(io.Discard, os.Stdin)
			close(stdinClosed)
		}()
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	startServer := func(serve func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- serve(ctx)
		}()
	}

	if cfg.PostgresListenAddr != "" {
		pgSrv := proxy.NewServer(cfg.PostgresListenAddr, logger, redashClient, registry, cfg.Username, cfg.Password)
		startServer(pgSrv.ListenAndServe)
	} else {
		logger.Info("PostgreSQL listener disabled (no postgres_listen_addr configured)")
	}

	if cfg.MySQLListenAddr != "" {
		mysqlSrv := mysqlwire.NewServer(cfg.MySQLListenAddr, logger, redashClient, registry, cfg.Username, cfg.Password)
		startServer(mysqlSrv.ListenAndServe)
	}

	var exitCode int
	select {
	case s := <-sigCh:
		logger.Info("shutting down", "signal", s.String())
	case <-stdinClosed:
		logger.Info("stdin closed, shutting down")
	case err := <-errCh:
		if err != nil {
			logger.Error("server error", "error", err)
			exitCode = 1
		}
	}
	cancel()

	// errCh is buffered so server goroutines never block on send after we stop reading it.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		logger.Warn("shutdown timed out, forcing exit")
	}

	logger.Info("server stopped")
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// warnDefaultCredentials loudly flags the well-known built-in credentials when a
// listener is reachable beyond loopback; that combination exposes the full Redash
// API key authority to anyone on the network.
func warnDefaultCredentials(logger *slog.Logger, cfg *config.Config) {
	if !cfg.UsesDefaultCredentials() {
		return
	}
	for _, addr := range []string{cfg.PostgresListenAddr, cfg.MySQLListenAddr} {
		if addr == "" {
			continue
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		// An empty host (e.g. ":15432") binds all interfaces, so treat anything that
		// isn't explicitly loopback as exposed.
		switch host {
		case "127.0.0.1", "localhost", "::1":
			// loopback only; nothing to warn about
		default:
			logger.Warn("listening on a non-loopback address with the built-in default credentials; set a username/password in your config",
				"addr", addr)
		}
	}
}

func printBanner(cfg *config.Config, configPath string, sources []redash.DataSource, session *redash.SessionInfo) {
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render("redash-wire")
	label := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	value := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("8")).
		Padding(0, 1)
	line := func(l, v string) string {
		return fmt.Sprintf("%s %s", label.Render(fmt.Sprintf("%-8s →", l)), value.Render(v))
	}

	colorPg := lipgloss.Color("39")     // blue
	colorMySQL := lipgloss.Color("208") // orange

	coloredBox := func(color lipgloss.Color) lipgloss.Style {
		return boxStyle.BorderForeground(color)
	}
	coloredTitle := func(s string, color lipgloss.Color) string {
		return lipgloss.NewStyle().Bold(true).Foreground(color).Render(s)
	}
	defaultTitle := func(s string) string {
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")).Render(s)
	}

	var redashLines []string
	if session != nil && session.User.Name != "" {
		redashLines = append(redashLines, line("User", fmt.Sprintf("%s (%s)", session.User.Name, session.User.Email)))
	}
	redashLines = append(redashLines, line("URL", cfg.RedashURL))
	if session != nil && session.ClientConfig.Version != "" {
		redashLines = append(redashLines, line("Version", session.ClientConfig.Version))
	}
	redashLines = append(redashLines, line("Sources", formatSourceCount(sources)))
	redashBox := boxStyle.Render(fmt.Sprintf("%s\n%s", defaultTitle("Redash"), strings.Join(redashLines, "\n")))

	var serverBoxes []string

	if cfg.PostgresListenAddr != "" {
		pgLines := serverConnLines(cfg.PostgresListenAddr, false, cfg.Username, cfg.Password, label, value)
		pgBox := coloredBox(colorPg).Render(fmt.Sprintf("%s\n%s", coloredTitle("PostgreSQL", colorPg), strings.Join(pgLines, "\n")))
		serverBoxes = append(serverBoxes, pgBox)
	}

	if cfg.MySQLListenAddr != "" {
		mysqlLines := serverConnLines(cfg.MySQLListenAddr, true, cfg.Username, cfg.Password, label, value)
		mysqlBox := coloredBox(colorMySQL).Render(fmt.Sprintf("%s\n%s", coloredTitle("MySQL", colorMySQL), strings.Join(mysqlLines, "\n")))
		serverBoxes = append(serverBoxes, mysqlBox)
	}

	// Stack boxes vertically when they don't fit; +2 accounts for the left margin.
	servers := lipgloss.JoinHorizontal(lipgloss.Top, serverBoxes...)
	termWidth := 80
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil {
		termWidth = w
	}
	if lipgloss.Width(servers)+2 > termWidth {
		servers = lipgloss.JoinVertical(lipgloss.Left, serverBoxes...)
	}

	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  %s %s  %s %s  %s %s\n", title,
		dim.Render(version),
		dim.Render("profile:"), value.Render(cfg.Profile),
		dim.Render("config:"), value.Render(shortenHome(configPath)))
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, lipgloss.NewStyle().MarginLeft(2).Render(redashBox))
	fmt.Fprintln(os.Stderr, lipgloss.NewStyle().MarginLeft(2).Render(servers))
	fmt.Fprintln(os.Stderr)
}

func serverConnLines(addr string, isMysql bool, username, password string, label, value lipgloss.Style) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	line := func(l, v string) string {
		return fmt.Sprintf("%s %s", label.Render(fmt.Sprintf("%-8s →", l)), value.Render(v))
	}

	connectCmd := fmt.Sprintf("psql -h %s -p %s -U %s", host, port, username)
	if isMysql {
		connectCmd = fmt.Sprintf("mysql -h %s -P %s -u %s -p", host, port, username)
	}

	return []string{
		line("Host", host),
		line("Port", port),
		line("User", username),
		line("Password", password),
		line("Connect", connectCmd),
	}
}

func formatSourceCount(sources []redash.DataSource) string {
	var pgCount, mysqlCount, otherCount int
	for _, ds := range sources {
		switch {
		case redash.IsPostgresCompatible(ds.Type):
			pgCount++
		case redash.IsMySQLCompatible(ds.Type):
			mysqlCount++
		default:
			otherCount++
		}
	}

	s := fmt.Sprintf("%d available", len(sources))
	var parts []string
	if pgCount > 0 {
		parts = append(parts, fmt.Sprintf("%d PostgreSQL", pgCount))
	}
	if mysqlCount > 0 {
		parts = append(parts, fmt.Sprintf("%d MySQL", mysqlCount))
	}
	if otherCount > 0 {
		parts = append(parts, fmt.Sprintf("%d other", otherCount))
	}
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s
}

func shortenHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if rel, ok := strings.CutPrefix(path, home); ok {
		return "~" + rel
	}
	return path
}
