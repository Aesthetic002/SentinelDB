package lab

import (
	"context"
	"math/rand"
	"time"

	"sentineldb/internal/protocol"
)

// Scenario is one way a run can unfold. Degrade scenarios end by killing the node that was
// leader when they began. The rest must not be read as "about to fail".
type Scenario struct {
	Name string
	// Degrades marks a scenario with warning signs followed by a failure.
	Degrades bool
	// KillsLeader marks a scenario that ends with the original leader dying.
	KillsLeader bool
	Run         func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error
}

// Events are the moments a run's analysis depends on, in Unix milliseconds.
type Events struct {
	FaultStartMs int64
	KillMs       int64
	Victim       string
}

func nowMs() int64 { return time.Now().UnixMilli() }

func uniform(r *rand.Rand, lo, hi float64) float64 { return lo + r.Float64()*(hi-lo) }

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ramp worsens a fault in small steps, the way a failing disk, an overheating machine or a
// congested link gets gradually worse before it stops altogether.
func ramp(ctx context.Context, c *Cluster, id string, d time.Duration, final protocol.ChaosSpec) error {
	const step = 200 * time.Millisecond
	start := time.Now()
	for {
		f := float64(time.Since(start)) / float64(d)
		if f >= 1 {
			return c.Chaos(id, &final)
		}
		cur := protocol.ChaosSpec{
			DelayMs:  int(f * float64(final.DelayMs)),
			JitterMs: int(f * float64(final.JitterMs)),
			DropRate: f * final.DropRate,
		}
		if err := c.Chaos(id, &cur); err != nil {
			return err
		}
		if err := sleep(ctx, step); err != nil {
			return err
		}
	}
}

func degrade(pick func(*rand.Rand) protocol.ChaosSpec) func(context.Context, *Cluster, string, *rand.Rand, *Events) error {
	return func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		d := time.Duration(uniform(r, 6, 10) * float64(time.Second))
		ev.FaultStartMs = nowMs()
		// Keep going if a status call fails: a struggling node is exactly what this is
		// for, and the kill at the end must happen regardless.
		_ = ramp(ctx, c, victim, d, pick(r))
		ev.KillMs = nowMs()
		c.Kill(victim)
		return sleep(ctx, 3*time.Second)
	}
}

func followerOf(c *Cluster, leader string, r *rand.Rand, n int) []string {
	var out []string
	for _, i := range r.Perm(len(c.IDs)) {
		if c.IDs[i] != leader && c.Nodes[c.IDs[i]].Alive() {
			out = append(out, c.IDs[i])
			if len(out) == n {
				break
			}
		}
	}
	return out
}

// Scenarios lists everything collection and evaluation run.
var Scenarios = []Scenario{
	// A node getting worse, then dying.
	{Name: "degrade-delay", Degrades: true, KillsLeader: true, Run: degrade(func(r *rand.Rand) protocol.ChaosSpec {
		d := uniform(r, 250, 450)
		return protocol.ChaosSpec{DelayMs: int(d), JitterMs: int(d * 0.2)}
	})},
	{Name: "degrade-jitter", Degrades: true, KillsLeader: true, Run: degrade(func(r *rand.Rand) protocol.ChaosSpec {
		return protocol.ChaosSpec{DelayMs: int(uniform(r, 50, 120)), JitterMs: int(uniform(r, 300, 450))}
	})},
	{Name: "degrade-loss", Degrades: true, KillsLeader: true, Run: degrade(func(r *rand.Rand) protocol.ChaosSpec {
		return protocol.ChaosSpec{DelayMs: 10, DropRate: uniform(r, 0.3, 0.6)}
	})},
	{Name: "degrade-mixed", Degrades: true, KillsLeader: true, Run: degrade(func(r *rand.Rand) protocol.ChaosSpec {
		return protocol.ChaosSpec{
			DelayMs: int(uniform(r, 120, 250)), JitterMs: int(uniform(r, 80, 180)), DropRate: uniform(r, 0.1, 0.35),
		}
	})},

	// A sudden death with no warning at all. Nothing can predict this one, so it stays out
	// of the "about to fail" class and shows what the predictor cannot do.
	{Name: "hard-kill", KillsLeader: true, Run: func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		ev.FaultStartMs = nowMs()
		if err := sleep(ctx, time.Duration(uniform(r, 3, 6)*float64(time.Second))); err != nil {
			return err
		}
		ev.KillMs = nowMs()
		c.Kill(victim)
		return sleep(ctx, 3*time.Second)
	}},

	// Things that look alarming but are no reason to change leader.
	{Name: "healthy", Run: func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		ev.FaultStartMs = nowMs()
		return sleep(ctx, 12*time.Second)
	}},
	{Name: "blip", Run: func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		ev.FaultStartMs = nowMs()
		for i := 0; i < 3; i++ {
			if err := sleep(ctx, time.Duration(uniform(r, 1, 3)*float64(time.Second))); err != nil {
				return err
			}
			spec := protocol.ChaosSpec{DelayMs: int(uniform(r, 60, 200)), JitterMs: int(uniform(r, 0, 60))}
			if err := c.Chaos(victim, &spec); err != nil {
				return err
			}
			if err := sleep(ctx, time.Duration(uniform(r, 0.3, 1.0)*float64(time.Second))); err != nil {
				return err
			}
			if err := c.Chaos(victim, nil); err != nil {
				return err
			}
		}
		return sleep(ctx, 2*time.Second)
	}},
	{Name: "steady-slow", Run: func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		// A consistently slow network is slow, not failing.
		ev.FaultStartMs = nowMs()
		spec := protocol.ChaosSpec{DelayMs: int(uniform(r, 30, 120)), JitterMs: int(uniform(r, 0, 10))}
		if err := c.Chaos(victim, &spec); err != nil {
			return err
		}
		return sleep(ctx, 12*time.Second)
	}},
	{Name: "steady-slow-high", Run: func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		// The same again, at latencies as bad as the worst of a dying node's. Without these,
		// a model can score well by simply learning "over 100ms means failure", which is
		// false: a slow network is not a failing node, however slow it is.
		ev.FaultStartMs = nowMs()
		spec := protocol.ChaosSpec{DelayMs: int(uniform(r, 120, 320)), JitterMs: int(uniform(r, 0, 15))}
		if err := c.Chaos(victim, &spec); err != nil {
			return err
		}
		return sleep(ctx, 12*time.Second)
	}},
	{Name: "one-bad-link", Run: func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		// One follower's link is terrible. That is the follower's problem, not the leader's.
		ev.FaultStartMs = nowMs()
		spec := protocol.ChaosSpec{
			DelayMs: int(uniform(r, 200, 450)), JitterMs: int(uniform(r, 0, 100)), DropRate: uniform(r, 0, 0.5),
			OnlyTo: followerOf(c, victim, r, 1)[0],
		}
		if err := c.Chaos(victim, &spec); err != nil {
			return err
		}
		return sleep(ctx, 12*time.Second)
	}},
	{Name: "follower-kill", Run: func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		ev.FaultStartMs = nowMs()
		if err := sleep(ctx, time.Duration(uniform(r, 2, 4)*float64(time.Second))); err != nil {
			return err
		}
		c.Kill(followerOf(c, victim, r, 1)[0])
		return sleep(ctx, 9*time.Second)
	}},
	{Name: "two-follower-kill", Run: func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		// Down to a bare majority: the leader is fine, and leaving would help nothing.
		ev.FaultStartMs = nowMs()
		if err := sleep(ctx, time.Duration(uniform(r, 2, 4)*float64(time.Second))); err != nil {
			return err
		}
		for _, f := range followerOf(c, victim, r, 2) {
			c.Kill(f)
		}
		return sleep(ctx, 9*time.Second)
	}},
}

// rampFor builds a kill-at-the-end degradation of a fixed duration, using the delay and
// jitter fault the model trained on.
func rampFor(d time.Duration) func(context.Context, *Cluster, string, *rand.Rand, *Events) error {
	return func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		delay := uniform(r, 250, 450)
		ev.FaultStartMs = nowMs()
		_ = ramp(ctx, c, victim, d, protocol.ChaosSpec{DelayMs: int(delay), JitterMs: int(delay * 0.2)})
		ev.KillMs = nowMs()
		c.Kill(victim)
		return sleep(ctx, 3*time.Second)
	}
}

// UnseenScenarios are never used for training, and test what the model does outside the
// conditions it learned from. They answer the questions a training-set score cannot.
var UnseenScenarios = []Scenario{
	// The same ramp as a real failure, except the node then recovers. Up to the moment of
	// recovery this is indistinguishable from dying, so a handover here is expected. What
	// matters is what being wrong costs.
	{Name: "ramp-recover", Run: func(ctx context.Context, c *Cluster, victim string, r *rand.Rand, ev *Events) error {
		delay := uniform(r, 250, 450)
		ev.FaultStartMs = nowMs()
		_ = ramp(ctx, c, victim, time.Duration(uniform(r, 6, 10)*float64(time.Second)),
			protocol.ChaosSpec{DelayMs: int(delay), JitterMs: int(delay * 0.2)})
		_ = c.Chaos(victim, nil)
		return sleep(ctx, 4*time.Second)
	}},
	// Far slower than anything in training: is the warning still recognized?
	{Name: "slow-ramp", Degrades: true, KillsLeader: true, Run: rampFor(25 * time.Second)},
	// Faster than the time the rule needs to act, so prediction should fall back to Raft.
	{Name: "fast-ramp", Degrades: true, KillsLeader: true, Run: rampFor(1500 * time.Millisecond)},
}

// ScenarioByName finds a scenario.
func ScenarioByName(name string) *Scenario {
	for i := range Scenarios {
		if Scenarios[i].Name == name {
			return &Scenarios[i]
		}
	}
	for i := range UnseenScenarios {
		if UnseenScenarios[i].Name == name {
			return &UnseenScenarios[i]
		}
	}
	return nil
}
