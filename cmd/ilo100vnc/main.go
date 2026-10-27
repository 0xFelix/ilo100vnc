// Command ilo100vnc exposes the HP iLO 100 remote console as VNC and as an
// embedded noVNC web client.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/0xFelix/ilo100vnc/internal/ilo"
	"github.com/0xFelix/ilo100vnc/internal/ipmi"
	"github.com/0xFelix/ilo100vnc/internal/rfb"
	"github.com/0xFelix/ilo100vnc/internal/web"
)

var version = "dev"

// config holds all settings. Precedence: flags > environment > config file.
type config struct {
	Host          string `json:"host"`
	User          string `json:"user"`
	Password      string `json:"password"`
	PasswordFile  string `json:"password_file"`
	Listen        string `json:"listen"`
	VNC           string `json:"vnc"`
	VNCPassword   string `json:"vnc_password"`
	IdleTimeout   string `json:"idle_timeout"`
	RelativeMouse bool   `json:"relative_mouse"`
	Debug         bool   `json:"debug"`
}

const envPrefix = "ILO100VNC_"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, showVersion, err := loadConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if showVersion {
		fmt.Println("ilo100vnc", version)
		return nil
	}

	logLevel := slog.LevelInfo
	if cfg.Debug {
		logLevel = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(log)

	iloCfg, err := buildConsoleConfig(cfg, log)
	if err != nil {
		return err
	}
	if cfg.Listen == "" && cfg.VNC == "" {
		return errors.New("nothing to serve: both -listen and -vnc are empty")
	}

	console := ilo.NewConsole(iloCfg)
	srv := rfb.NewServer(rfb.Options{
		Display:  console.Framebuffer(),
		Input:    console,
		Name:     "iLO 100 " + iloCfg.URL.Host,
		Password: cfg.VNCPassword,
		Log:      log,
		OnAttach: console.Attach,
		OnDetach: console.Detach,
		Power:    powerControl(ipmi.New(iloCfg.URL.Hostname(), ipmi.Port, iloCfg.User, iloCfg.Password, log)),
	})
	console.OnUpdate(func(u ilo.Update) {
		if u.Resized {
			srv.Resize()
		}
		if !u.Dirty.Empty() {
			srv.Damage(u.Dirty)
		}
	})
	if cfg.VNCPassword == "" {
		for _, a := range []string{cfg.Listen, cfg.VNC} {
			if a != "" && !isLoopback(a) {
				log.Warn("serving the console without a VNC password on a non-loopback address", "addr", a)
				break
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 2)

	var httpSrv *http.Server
	if cfg.Listen != "" {
		httpSrv = &http.Server{
			Addr:              cfg.Listen,
			Handler:           web.Handler(srv.ServeConn, log),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			log.Info("serving noVNC", "url", "http://"+displayAddr(cfg.Listen)+"/")
			if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	var vncLn net.Listener
	if cfg.VNC != "" {
		if vncLn, err = net.Listen("tcp", cfg.VNC); err != nil {
			return err
		}
		log.Info("serving VNC", "addr", vncLn.Addr().String())
		go func() {
			if err := srv.Serve(vncLn); !errors.Is(err, net.ErrClosed) {
				errc <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err = <-errc:
	}
	if vncLn != nil {
		_ = vncLn.Close()
	}
	if httpSrv != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = httpSrv.Shutdown(sctx)
		cancel()
	}
	console.Close()
	return err
}

// loadConfig builds the configuration from defaults, the config file, the
// environment and the command line, in increasing precedence.
func loadConfig(args []string) (config, bool, error) {
	cfg := config{Listen: "localhost:8080", User: "admin", IdleTimeout: "30s"}
	fs := flag.NewFlagSet("ilo100vnc", flag.ContinueOnError)
	configFile := fs.String("config", "", "JSON config file")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.StringVar(&cfg.Host, "host", cfg.Host, "iLO host name or address, e.g. 10.0.0.5 (or an http:// URL)")
	fs.StringVar(&cfg.User, "user", cfg.User, "iLO user name")
	fs.StringVar(&cfg.PasswordFile, "password-file", "", "file containing the iLO password (or set "+envPrefix+"PASSWORD)")
	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "HTTP listen address for noVNC and WebSocket (empty disables)")
	fs.StringVar(&cfg.VNC, "vnc", "", "VNC (RFB) listen address, e.g. localhost:5900 (empty disables)")
	fs.StringVar(&cfg.IdleTimeout, "idle-timeout", cfg.IdleTimeout, "close the iLO session this long after the last viewer left (0 = never)")
	fs.BoolVar(&cfg.RelativeMouse, "relative-mouse", false, "send relative instead of absolute mouse movements")
	fs.BoolVar(&cfg.Debug, "debug", false, "verbose logging with hex dumps of the iLO protocol")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: ilo100vnc -host <ip> [-user admin] [-listen localhost:8080] [-vnc localhost:5900]\n\n")
		fs.PrintDefaults()
		fmt.Fprintf(fs.Output(), "\nEnvironment: %sHOST, %sUSER, %sPASSWORD, %sVNC_PASSWORD, ... (upper-case config keys)\n",
			envPrefix, envPrefix, envPrefix, envPrefix)
	}
	// The flags are parsed twice: first to find -config, then again after
	// the config file and the environment, so that they take precedence.
	if err := fs.Parse(args); err != nil || *showVersion {
		return cfg, *showVersion, err
	}
	if *configFile != "" {
		if err := loadFile(*configFile, &cfg); err != nil {
			return cfg, false, err
		}
	}
	if err := loadEnv(fs, &cfg); err != nil {
		return cfg, false, err
	}
	return cfg, false, fs.Parse(args)
}

func loadFile(path string, cfg *config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	return nil
}

// loadEnv sets every flag from ILO100VNC_<NAME> (e.g. -password-file from
// ILO100VNC_PASSWORD_FILE) and the passwords, which have no flags to keep
// them out of ps.
func loadEnv(fs *flag.FlagSet, cfg *config) error {
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "config" || f.Name == "version" {
			return
		}
		key := envPrefix + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v, ok := os.LookupEnv(key); ok {
			if err := fs.Set(f.Name, v); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
			}
		}
	})
	for key, p := range map[string]*string{"PASSWORD": &cfg.Password, "VNC_PASSWORD": &cfg.VNCPassword} {
		if v, ok := os.LookupEnv(envPrefix + key); ok {
			*p = v
		}
	}
	return errors.Join(errs...)
}

func buildConsoleConfig(cfg config, log *slog.Logger) (ilo.Config, error) {
	if cfg.Host == "" {
		return ilo.Config{}, errors.New("missing -host")
	}
	if !strings.Contains(cfg.Host, "://") {
		cfg.Host = "http://" + cfg.Host
	}
	u, err := url.Parse(cfg.Host)
	if err == nil && u.Scheme == "https" {
		return ilo.Config{}, errors.New("-host must use http://: the iLO 100 only offers SSLv3/TLS 1.0 with DHE-DSS-3DES, which Go does not support; keep the iLO on an isolated network")
	}
	if err != nil || u.Host == "" || u.Scheme != "http" {
		return ilo.Config{}, fmt.Errorf("invalid -host %q", cfg.Host)
	}
	if cfg.PasswordFile != "" {
		b, err := os.ReadFile(cfg.PasswordFile)
		if err != nil {
			return ilo.Config{}, err
		}
		cfg.Password = strings.TrimRight(string(b), "\r\n")
	}
	if cfg.Password == "" {
		return ilo.Config{}, fmt.Errorf("missing iLO password: set %sPASSWORD, -password-file or the config file", envPrefix)
	}
	idle, err := time.ParseDuration(cfg.IdleTimeout)
	if err != nil {
		return ilo.Config{}, fmt.Errorf("invalid idle timeout: %w", err)
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return ilo.Config{
		URL:           u,
		User:          cfg.User,
		Password:      cfg.Password,
		HTTPClient:    &http.Client{Transport: tr, Timeout: 20 * time.Second},
		IdleTimeout:   idle,
		RelativeMouse: cfg.RelativeMouse,
		Log:           log,
		Debug:         cfg.Debug,
	}, nil
}

// powerControl maps viewer power operations (XVP) to IPMI chassis commands.
func powerControl(c *ipmi.Client) func(rfb.PowerOp) error {
	return func(op rfb.PowerOp) error {
		ctx := context.Background()
		switch op {
		case rfb.PowerShutdown:
			return c.SoftOff(ctx)
		case rfb.PowerReboot:
			return c.PowerCycle(ctx)
		case rfb.PowerReset:
			return c.Reset(ctx)
		case rfb.PowerOn:
			return c.PowerOn(ctx)
		case rfb.PowerBIOSSetup:
			return c.BIOSSetup(ctx)
		}
		return fmt.Errorf("unsupported power operation %v", op)
	}
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}
