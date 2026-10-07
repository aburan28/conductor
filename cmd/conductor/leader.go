package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"

	"github.com/aburan28/conductor/internal/config"
	"github.com/aburan28/conductor/internal/db"
)

type leaderConfig struct {
	database, dsn, dsnSource string
	endpoint, project        string
	bootstrap                bool
	daemonArgs               []string
}

// parseLeaderConfig resolves the same secret references as conductord, while leaving
// unspecified server flags to the daemon's config/environment precedence.
func parseLeaderConfig(args []string, output io.Writer) (*leaderConfig, error) {
	c := &leaderConfig{}
	fs := flag.NewFlagSet("leader", flag.ContinueOnError)
	fs.SetOutput(output)
	mode := os.Getenv("CONDUCTOR_DATABASE_MODE")
	if mode == "" {
		mode = "local"
	}
	fs.StringVar(&c.database, "database", mode, "local: manage loopback Postgres; external or rds: use the configured database without Docker")
	dsnFlag := fs.String("dsn", "", "database connection string (prefer DATABASE_URL or database.url_env/url_file in --config)")
	configPath := fs.String("config", os.Getenv("CONDUCTOR_CONFIG"), "conductord YAML configuration file")
	fs.BoolVar(&c.bootstrap, "bootstrap", false, "initialize this operator's project and login before serving")
	fs.StringVar(&c.project, "project", "", "project slug used with --bootstrap")
	fs.String("addr", "", "API listen address (daemon default: 127.0.0.1:8080)")
	fs.String("public-url", "", "advertised API URL; does not change where the local leader starts")
	fs.String("tls-cert", "", "API TLS certificate file")
	fs.String("tls-key", "", "API TLS private key file")
	fs.String("security-mode", "enhanced", "authentication mode (default: enhanced)")
	fs.Bool("behind-proxy", false, "trust a proxy you control that rewrites forwarding headers")
	fs.String("nat-mode", "", "off, upnp or tailscale (daemon/config default: off)")
	fs.String("nat-internal-ip", "", "LAN address UPnP forwards the API port to")
	fs.String("nat-external-port", "", "UPnP external API TCP port")
	fs.String("nat-lease", "", "UPnP mapping lease duration")
	fs.String("nat-timeout", "", "NAT discovery/operation timeout")
	fs.String("nat-https-port", "", "Tailscale HTTPS listener port")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, errors.New("leader takes flags, not positional arguments")
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	fileValues := map[string]string{}
	if *configPath != "" {
		file, err := config.LoadServerFile(*configPath, os.Getenv)
		if err != nil {
			return nil, err
		}
		fileValues = file.FlagValues()
		if !given["database"] && os.Getenv("CONDUCTOR_DATABASE_MODE") == "" && fileValues["database"] != "" {
			c.database = fileValues["database"]
		}
	}
	switch c.database {
	case "local", "external", "rds":
	default:
		return nil, errors.New("--database must be local, external or rds")
	}
	c.dsn = *dsnFlag
	if c.dsn == "" {
		c.dsn = os.Getenv("DATABASE_URL")
	}
	if c.dsn == "" {
		c.dsn = fileValues["dsn"]
	}
	if c.database == "local" {
		var err error
		c.dsn, c.dsnSource, err = resolveDSN(c.dsn, "")
		if err != nil {
			return nil, err
		}
		u, err := url.Parse(c.dsn)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || !isLoopbackHost(u.Hostname()) {
			return nil, errors.New("local database mode needs a loopback PostgreSQL URL; use --database external for another database")
		}
	} else if c.dsn == "" {
		return nil, errors.New("external and RDS database modes require --dsn, DATABASE_URL, or database.url_env/url_file in --config")
	}
	if err := db.ValidateDSN(c.database, c.dsn); err != nil {
		return nil, err
	}
	c.daemonArgs = []string{"--database", c.database, "--security-mode", fs.Lookup("security-mode").Value.String()}
	if *configPath != "" {
		c.daemonArgs = append(c.daemonArgs, "--config", *configPath)
	}
	var names []string
	for name := range given {
		switch name {
		case "database", "dsn", "config", "bootstrap", "project", "security-mode":
		default:
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		// = also preserves an explicit --behind-proxy=false.
		c.daemonArgs = append(c.daemonArgs, "--"+name+"="+fs.Lookup(name).Value.String())
	}
	value := func(name, env, fallback string) string {
		if given[name] {
			return fs.Lookup(name).Value.String()
		}
		if v := os.Getenv(env); v != "" {
			return v
		}
		if v := fileValues[name]; v != "" {
			return v
		}
		return fallback
	}
	c.endpoint = value("public-url", "CONDUCTOR_PUBLIC_URL", "")
	if c.endpoint == "" {
		addr := value("addr", "CONDUCTOR_ADDR", "127.0.0.1:8080")
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, errors.New("--addr must have a host and port")
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		scheme := "http"
		if value("tls-cert", "CONDUCTOR_TLS_CERT", "") != "" {
			scheme = "https"
		}
		c.endpoint = scheme + "://" + net.JoinHostPort(host, port)
	}
	return c, nil
}

// cmdLeader runs one configured control-plane leader in the foreground. Workers talk
// to its authenticated API; no database credentials are passed to workers.
func cmdLeader(ctx context.Context, args []string) error {
	c, err := parseLeaderConfig(args, os.Stderr)
	if err != nil {
		return err
	}
	daemon, err := locateDaemon()
	if err != nil {
		return err
	}
	// Reject invalid TLS/NAT/server settings before starting a local database.
	checkArgs := append([]string{"config", "check"}, c.daemonArgs...)
	check := daemonCommand(daemon, c.dsn, checkArgs...)
	check.Stdout, check.Stderr = io.Discard, os.Stderr
	if err := check.Run(); err != nil {
		return fmt.Errorf("leader configuration check: %w", err)
	}
	if c.database == "local" {
		if err := ensureDatabase(c.dsn); err != nil {
			return err
		}
		if c.dsnSource == dsnGenerated {
			c.dsn, err = settleGeneratedDSN(ctx, c.dsn)
			if err != nil {
				return err
			}
			if err := saveDSN(c.dsn); err != nil {
				return fmt.Errorf("saving the database DSN: %w", err)
			}
		}
	}
	if c.bootstrap {
		bootstrapArgs := []string{"bootstrap", "--endpoint", c.endpoint}
		if c.project != "" {
			bootstrapArgs = append(bootstrapArgs, "--project", c.project)
		}
		bootstrap := daemonCommand(daemon, c.dsn, bootstrapArgs...)
		bootstrap.Stdin, bootstrap.Stdout, bootstrap.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := bootstrap.Run(); err != nil {
			return fmt.Errorf("leader bootstrap: %w", err)
		}
	}
	cmd := leaderCommand(ctx, daemon, c.dsn, c.daemonArgs)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("leader: %w", err)
	}
	return nil
}

func leaderCommand(ctx context.Context, daemon, dsn string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, daemon, args...)
	cmd.Env = daemonCommand(daemon, dsn).Env
	// Give conductord time to release NAT mappings and drain requests when the
	// supervising CLI receives SIGINT/SIGTERM. Never turn cancellation into an immediate kill.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 35 * time.Second
	return cmd
}
