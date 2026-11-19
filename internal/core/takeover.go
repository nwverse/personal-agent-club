package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider/grokbot"
	"github.com/nwverse/personal-agent-club/internal/takeover"
)

// How many takeovers may wait at once, and how long one stays.
const (
	takeoversWaiting = 2
	takeoverTTL      = 30 * time.Minute
)

// takeover is one linking of a Grok Bot's own machine. The box side is driven through the bot; the hub side is only
// a plan until the user confirms. Nothing kept here is a secret.
type takeoverRun struct {
	accountID string
	createdAt time.Time
	report    takeover.Report
	// asked is the phase the bot was last asked to carry out, so a poll does not ask twice for the same one.
	asked string
	// shown is true once the plan was handed back to the caller. Apply refuses without it.
	shown   bool
	plan    takeover.Plan
	applied bool
}

func (s *Space) takeoverRuns() map[string]*takeoverRun {
	if s.takeovers == nil {
		s.takeovers = map[string]*takeoverRun{}
	}
	return s.takeovers
}

func (s *Space) sweepTakeovers(now time.Time) {
	for id, t := range s.takeovers {
		if now.Sub(t.createdAt) > takeoverTTL {
			delete(s.takeovers, id)
		}
	}
}

// StartTakeover is club.grok_bot.takeover.start: begin linking that Grok Bot account's own machine. It does no work
// on the box yet; the first status poll asks the bot's agent for the first missing step.
func (s *Space) StartTakeover(ctx context.Context, p protocol.TakeoverStartParams) (protocol.TakeoverStart, error) {
	a, err := s.requireAccount(ctx, p.AccountID)
	if err != nil {
		return protocol.TakeoverStart{}, err
	}
	if a.Provider != grokbot.Name {
		return protocol.TakeoverStart{}, protocol.Unsupported("only a grok_bot account has a machine to link").Because(protocol.ReasonProviderUnavailable)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.club.opts.Now()
	s.sweepTakeovers(now)
	if len(s.takeoverRuns()) >= takeoversWaiting {
		return protocol.TakeoverStart{}, protocol.Conflict("two takeovers are already waiting").Because(protocol.ReasonLoginLimit)
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return protocol.TakeoverStart{}, err
	}
	id := "t" + hex.EncodeToString(b[:])
	s.takeoverRuns()[id] = &takeoverRun{accountID: p.AccountID, createdAt: now}
	return protocol.TakeoverStart{TakeoverID: id, Phase: takeover.PhaseNode, Detail: "waiting to ask the bot"}, nil
}

// TakeoverStatus is club.grok_bot.takeover.status. Where the box still has a step to do, and that step has not been
// asked yet, it asks the bot's agent and reads back the one line it answers.
func (s *Space) TakeoverStatus(ctx context.Context, p protocol.TakeoverParams) (protocol.TakeoverStatus, error) {
	accountID, phase, node, err := s.takeoverNext(ctx, p.TakeoverID)
	if err != nil {
		return protocol.TakeoverStatus{}, err
	}
	if phase == "" {
		return s.takeoverView(ctx, p.TakeoverID)
	}
	line, err := s.askBot(ctx, accountID, phase, node)
	if err != nil {
		return protocol.TakeoverStatus{}, err
	}
	report, err := takeover.ParseReport(line)
	if err != nil {
		return protocol.TakeoverStatus{}, protocol.Invalid(err.Error())
	}
	return s.takeoverRecord(ctx, p.TakeoverID, phase, report)
}

// takeoverNext reports the step still to ask for (phase "" when there is nothing to ask).
func (s *Space) takeoverNext(ctx context.Context, id string) (accountID, phase, node string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.liveTakeover(id)
	if err != nil {
		return "", "", "", err
	}
	phase = t.report.Phase()
	if phase == "" {
		phase = takeover.PhaseNode
	}
	if phase == takeover.PhasePlan || phase == takeover.PhaseLinked {
		return t.accountID, "", "", nil
	}
	return t.accountID, phase, t.report.Node, nil
}

func (s *Space) takeoverRecord(ctx context.Context, id, asked string, report takeover.Report) (protocol.TakeoverStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.liveTakeover(id)
	if err != nil {
		return protocol.TakeoverStatus{}, err
	}
	t.report, t.asked = report, asked
	return viewOf(t), nil
}

func (s *Space) takeoverView(ctx context.Context, id string) (protocol.TakeoverStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.liveTakeover(id)
	if err != nil {
		return protocol.TakeoverStatus{}, err
	}
	return viewOf(t), nil
}

func viewOf(t *takeoverRun) protocol.TakeoverStatus {
	phase := t.report.Phase()
	if phase == "" {
		phase = takeover.PhaseNode
	}
	st := protocol.TakeoverStatus{Phase: phase, Detail: t.report.Detail, Node: t.report.NodeReady,
		Paired: t.report.Paired, SSH: t.report.SSHReady}
	st.Waiting = phase != takeover.PhasePlan && phase != takeover.PhaseLinked
	if t.shown {
		st.PlanID = t.plan.ID
	}
	return st
}

// TakeoverPlan is club.grok_bot.takeover.plan: describe the hub change and remember that it was shown. Nothing is
// written.
func (s *Space) TakeoverPlan(ctx context.Context, p protocol.TakeoverParams) (protocol.TakeoverPlan, error) {
	node, err := s.takeoverNode(ctx, p.TakeoverID)
	if err != nil {
		return protocol.TakeoverPlan{}, err
	}
	path, bin := s.managerPath(node)
	if path == "" {
		return protocol.TakeoverPlan{}, protocol.Unsupported("the hub's persona directory is not configured").Because(protocol.ReasonProviderUnavailable)
	}
	plan, err := takeover.Prepare(path, node, bin)
	if err != nil {
		return protocol.TakeoverPlan{}, protocol.Invalid(err.Error())
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return protocol.TakeoverPlan{}, err
	}
	plan.ID = "p" + hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.liveTakeover(p.TakeoverID)
	if err != nil {
		return protocol.TakeoverPlan{}, err
	}
	t.plan, t.shown = plan, true
	return protocol.TakeoverPlan{PlanID: plan.ID, Summary: plan.Summary, Node: plan.Node, Kind: plan.Kind,
		Host: plan.SSHHost, AlreadyLinked: plan.AlreadyLinked}, nil
}

// ApplyTakeover is club.grok_bot.takeover.apply. It writes the machine list only after the plan was shown, and it
// never restarts the hub: the answer says so.
func (s *Space) ApplyTakeover(ctx context.Context, p protocol.TakeoverParams) (protocol.TakeoverApply, error) {
	plan, path, err := s.takeoverReady(ctx, p.TakeoverID)
	if err != nil {
		return protocol.TakeoverApply{}, err
	}
	if plan.ID == "" {
		return protocol.TakeoverApply{Applied: false, Detail: "not confirmed: the plan was not shown"}, nil
	}
	if plan.AlreadyLinked {
		return protocol.TakeoverApply{Applied: false, Detail: "already linked; nothing written"}, nil
	}
	backup, err := takeover.Apply(path, plan, s.club.opts.Now())
	if err != nil {
		return protocol.TakeoverApply{}, protocol.Conflict(err.Error())
	}
	s.mu.Lock()
	t, lockErr := s.liveTakeover(p.TakeoverID)
	if lockErr == nil {
		t.applied = true
	}
	s.mu.Unlock()
	return protocol.TakeoverApply{Applied: true, Detail: "written; the hub was not restarted", Backup: backup}, nil
}

// takeoverReady hands back the plan to write. An empty plan means it was never shown, so nothing may be written.
func (s *Space) takeoverReady(ctx context.Context, id string) (takeover.Plan, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.liveTakeover(id)
	if err != nil {
		return takeover.Plan{}, "", err
	}
	if !t.shown {
		return takeover.Plan{}, "", nil
	}
	if t.applied {
		return takeover.Plan{}, "", protocol.Conflict("already written")
	}
	path, _ := s.managerPath(t.plan.Node)
	return t.plan, path, nil
}

// takeoverNode is the node name once the box side is done.
func (s *Space) takeoverNode(ctx context.Context, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.liveTakeover(id)
	if err != nil {
		return "", err
	}
	if t.report.Phase() != takeover.PhasePlan && t.report.Phase() != takeover.PhaseLinked {
		return "", protocol.Conflict("the box is not ready yet")
	}
	if t.report.Node == "" {
		return "", protocol.Invalid("the box did not name its node")
	}
	return t.report.Node, nil
}

func (s *Space) liveTakeover(id string) (*takeoverRun, error) {
	s.sweepTakeovers(s.club.opts.Now())
	t := s.takeoverRuns()[id]
	if t == nil {
		return nil, protocol.NotFound("no such takeover").Because(protocol.ReasonLoginNotFound)
	}
	return t, nil
}

// askBot hands one step to the bot's own agent and returns the one line it answered.
func (s *Space) askBot(ctx context.Context, accountID, phase, node string) (string, error) {
	step, err := takeover.Step(phase, node)
	if err != nil {
		return "", protocol.Invalid(err.Error())
	}
	drive := s.club.opts.Takeover
	if drive == nil {
		return "", protocol.Unsupported("this build cannot drive the bot's machine").Because(protocol.ReasonProviderUnavailable)
	}
	return drive(ctx, accountID, step)
}

// managerPath is the hub's machine list and the remote binary an entry should name. Both come from the process's
// options, never from the bot, so a report cannot point the write somewhere else.
func (s *Space) managerPath(node string) (string, string) {
	dir := s.club.opts.PersonaDir
	if dir == "" {
		return "", ""
	}
	return filepath.Join(dir, "managers", "default", "manager.json"),
		filepath.Join("/home", node, ".persona", "bin", "persona-node")
}
