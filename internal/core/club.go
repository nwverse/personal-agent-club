// Package core is club itself, behind both shells: spaces, accounts, one sync round, the outbox, read state, search
// and change events. Both the Persona extension shell and the serve shell call it through Club.Call, so they answer
// the same protocol by construction.
package core

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/store"
	"github.com/nwverse/personal-agent-club/internal/vault"
)

// DefaultFollowUps are when a sent message's session is synced again, and an unconfirmed send asked about.
var DefaultFollowUps = []time.Duration{4 * time.Second, 15 * time.Second, 45 * time.Second}

// Options configure a Club.
type Options struct {
	// Version is the program's version, reported by club.info.
	Version string
	// Registry holds the provider adapters (required).
	Registry *provider.Registry
	// OpenSpace opens a space's store and vault; nil keeps every space in memory.
	OpenSpace func(name string) (store.Store, vault.Vault, error)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// FollowUps are the follow-up delays; nil is DefaultFollowUps, an empty non-nil slice turns them off.
	FollowUps []time.Duration
	// Log receives operational messages (never credentials or message text); nil discards them.
	Log *slog.Logger
	// PollInterval is how often every open space's accounts are synced; 0 turns polling off (sync then happens on
	// pairing, on club.accounts.sync and after sends).
	PollInterval time.Duration
	// NoRecovery opens spaces without recovering their outboxes (claimed sends become unconfirmed, pending ones go
	// out): for a tool that opens a data directory offline (an import) and must never send. The next owner recovers.
	NoRecovery bool
	// WatchLimit is how long a provider's push stays open after a send; 0 is DefaultWatchLimit, negative turns
	// watching off.
	WatchLimit time.Duration
	// OnClose runs once, at the end of the first Close, when nothing the club runs is left: a shell releases there
	// what OpenSpace opened (a data directory). A later or concurrent Close returns after it.
	OnClose func()
	// Takeover drives one step on a Grok Bot's own machine and returns the one line it answered. Nil means this build
	// cannot (the takeover methods then say so). Tests set it; the extension shell sets the one that sends through the
	// bot's own session.
	Takeover func(ctx context.Context, accountID, step string) (string, error)
	// PersonaDir is the hub user's ~/.persona, where the machine list lives. Empty means the hub side cannot be
	// prepared. The bot never names this path.
	PersonaDir string
}

// DefaultWatchLimit is how long a provider's push stays open after a send, at most.
const DefaultWatchLimit = 90 * time.Second

// MinPollInterval is the shortest poll interval a shell should configure.
const MinPollInterval = 15 * time.Second

// DefaultPollInterval is the poll interval of both shells unless told otherwise.
const DefaultPollInterval = 60 * time.Second

// Club is one club: its spaces and their events.
type Club struct {
	opts   Options
	events broker
	ctx    context.Context
	stop   context.CancelFunc

	mu      sync.Mutex
	spaces  map[string]*Space
	timers  map[*time.Timer]struct{}
	closed  bool
	wg      sync.WaitGroup
	onClose sync.Once
}

// New is a club.
func New(opts Options) *Club {
	if opts.Registry == nil {
		opts.Registry = provider.NewRegistry()
	}
	if opts.OpenSpace == nil {
		opts.OpenSpace = func(string) (store.Store, vault.Vault, error) { return store.NewMemory(), vault.NewMemory(), nil }
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.FollowUps == nil {
		opts.FollowUps = DefaultFollowUps
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.WatchLimit == 0 {
		opts.WatchLimit = DefaultWatchLimit
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Club{opts: opts, ctx: ctx, stop: stop, spaces: map[string]*Space{}, timers: map[*time.Timer]struct{}{}}
}

// Registry is the club's providers.
func (c *Club) Registry() *provider.Registry { return c.opts.Registry }

// SetTakeover is how a takeover drives the Grok Bot's own machine. The extension shell sets it after New, because
// the driver needs the club it drives.
func (c *Club) SetTakeover(fn func(ctx context.Context, accountID, step string) (string, error)) {
	c.opts.Takeover = fn
}

// SetPersonaDir is the hub user's ~/.persona, read when a takeover prepares the machine list.
func (c *Club) SetPersonaDir(dir string) { c.opts.PersonaDir = dir }

func (c *Club) now() int64 { return c.opts.Now().UnixMilli() }

// Info is club.info's answer.
func (c *Club) Info() protocol.ClubInfo {
	return protocol.ClubInfo{Version: c.opts.Version, ProtocolVersion: protocol.Version,
		Providers: c.opts.Registry.List(), Browser: false}
}

// Space opens a space (once; later calls get the same one). Opening recovers its outbox: sends claimed before a
// restart become unconfirmed and are never sent again.
func (c *Club) Space(ctx context.Context, name string) (*Space, error) {
	if !ValidSpace(name) {
		return nil, protocol.Invalid(fmt.Sprintf("invalid space name %q", name))
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("club is closed")
	}
	if s := c.spaces[name]; s != nil {
		c.mu.Unlock()
		return s, nil
	}
	st, v, err := c.opts.OpenSpace(name)
	if err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("open space %s: %w", name, err)
	}
	s := newSpace(name, c, st, v)
	c.spaces[name] = s
	c.mu.Unlock()
	if !c.opts.NoRecovery {
		if err := s.recover(ctx); err != nil {
			c.opts.Log.Warn("outbox recovery failed", "space", name, "err", err)
		}
	}
	if c.opts.PollInterval > 0 {
		c.goTracked(func() { s.poll(c.ctx, c.opts.PollInterval) })
	}
	return s, nil
}

// goTracked runs f in the background; Close waits for it. f must return once the club's context ends.
func (c *Club) goTracked(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		f()
	}()
}

// Subscribe receives a space's change events ("" for every space).
func (c *Club) Subscribe(space string) *Subscription { return c.events.subscribe(space) }

func (c *Club) publish(e Event) { c.events.publish(e) }

// after runs f after d unless the club closes first.
func (c *Club) after(d time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	var t *time.Timer
	c.wg.Add(1)
	t = time.AfterFunc(d, func() {
		defer c.wg.Done()
		c.mu.Lock()
		_, live := c.timers[t]
		delete(c.timers, t)
		closed := c.closed
		c.mu.Unlock()
		if live && !closed {
			f()
		}
	})
	c.timers[t] = struct{}{}
}

// Close stops polling, follow-ups and watches, waits for those running, then runs Options.OnClose.
func (c *Club) Close() {
	c.stop()
	c.mu.Lock()
	c.closed = true
	for t := range c.timers {
		if t.Stop() {
			c.wg.Done()
		}
		delete(c.timers, t)
	}
	c.mu.Unlock()
	c.wg.Wait()
	c.onClose.Do(func() {
		if c.opts.OnClose != nil {
			c.opts.OnClose()
		}
	})
}
