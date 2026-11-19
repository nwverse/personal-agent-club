package extension

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/datadir"
	"github.com/nwverse/personal-agent-club/internal/providers"
)

// Where the Persona shell keeps its data. The hub gives every extension one data directory (PERSONA_EXTENSION_DATA,
// also HOME inside the sandbox, deleted on uninstall unless kept); club's lives there, as a data directory of the
// datadir package with the key under its own name:
//
//	<data>/club.key                        the key of the credential files (vault.LoadKey's format), 0600
//	<data>/club.lock                       held while the program runs (an offline import is refused meanwhile)
//	<data>/spaces/default/club.db          the one space this shell serves: SQLite (WAL)
//	<data>/spaces/default/credentials.enc  its credentials, sealed with the key
const (
	KeyFileName = "club.key"
	SpacesDir   = "spaces"
)

// Host is what the Persona shell hands the core: where its data goes, and the HTTP client every provider must use.
type Host struct {
	// DataDir is the extension's data directory; "" when the program does not run under a hub (tests, a terminal).
	DataDir string
	// KeyFile is the credential key's file in DataDir; "" without DataDir.
	KeyFile string
	// HTTP goes out through the hub's egress proxy when there is one: inside the sandbox it is the only way out.
	HTTP *http.Client
}

// SpaceDir is a space's directory under DataDir.
func (h Host) SpaceDir(space string) string { return filepath.Join(h.DataDir, SpacesDir, space) }

// Configure fills in opts from h: the providers (cue, grok_bot, groq) on h.HTTP, and, with a data directory, the
// spaces on disk: the directory is opened (and locked) here and closed when the club closes (opts.OnClose). Without
// one every space stays in memory and is empty again after a restart.
func Configure(h Host, opts *core.Options, log *slog.Logger) error {
	cfg := providers.FromEnv()
	if cfg.Proxy != "" {
		log.Warn("ignoring " + providers.EnvProxy + ": under the hub every provider request goes through the hub's egress proxy")
	}
	cfg.Proxy, cfg.Client = "", h.HTTP
	reg, err := providers.Registry(cfg)
	if err != nil {
		return err
	}
	opts.Registry = reg
	if h.DataDir == "" {
		log.Warn("no extension data directory (PERSONA_EXTENSION_DATA): keeping everything in memory")
		return nil
	}
	dir, err := datadir.Open(h.DataDir, h.KeyFile)
	if err != nil {
		return err
	}
	opts.OpenSpace = dir.OpenSpace
	opts.OnClose = func() {
		if err := dir.Close(); err != nil {
			log.Warn("closing the data directory failed", "err", err)
		}
	}
	return nil
}

// NewClub is the club the Persona shell serves, built from the program's environment: the data directory the hub
// gave (if any) and the client that goes through its proxy.
func NewClub(version string, log *slog.Logger) (*core.Club, Host, error) {
	h := Host{DataDir: ext.Env().Data, HTTP: HTTPClient()}
	if h.DataDir != "" {
		h.KeyFile = filepath.Join(h.DataDir, KeyFileName)
	}
	opts := core.Options{Version: version, Log: log, PollInterval: core.DefaultPollInterval}
	if err := Configure(h, &opts, log); err != nil {
		return nil, h, fmt.Errorf("storage: %w", err)
	}
	club := core.New(opts)
	club.SetTakeover(driveTakeover(club))
	if home, err := os.UserHomeDir(); err == nil {
		club.SetPersonaDir(filepath.Join(home, ".persona"))
	}
	return club, h, nil
}

// HTTPClient is the client providers use. Inside the hub's sandbox the only way out is the egress proxy the hub
// names in HTTPS_PROXY / HTTP_PROXY (and only to the hosts in permissions.network), so the transport takes its proxy
// from the environment, as http.DefaultTransport does; a transport built from scratch without Proxy would dial
// directly and be refused by the sandbox. No overall timeout: a provider sets its own per request.
func HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}
