package predict

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sentineldb/internal/raft"
)

var t0 = time.Unix(1000, 0)

// feed simulates heartbeats every 50ms to a peer over [start, start+d) with round trips
// given by rtt. A negative rtt means the reply never comes.
func feed(m *Monitor, peer raft.NodeID, start time.Time, d time.Duration, rtt func(time.Time) time.Duration) {
	for t := start; t.Before(start.Add(d)); t = t.Add(50 * time.Millisecond) {
		m.HeartbeatSent(peer, t)
		if r := rtt(t); r >= 0 {
			m.HeartbeatAcked(peer, t, t.Add(r))
		}
	}
}

func constant(d time.Duration) func(time.Time) time.Duration {
	return func(time.Time) time.Duration { return d }
}

func fiveNodes() raft.Membership {
	return raft.Membership{"n1": "", "n2": "", "n3": "", "n4": "", "n5": ""}
}

func newLeader(t *testing.T, model *Model) *Monitor {
	t.Helper()
	m, err := NewMonitor(MonitorConfig{ID: "n1", Model: model})
	if err != nil {
		t.Fatal(err)
	}
	m.LeadershipChanged(true, t0)
	return m
}

func TestFeaturesOnSteadyLink(t *testing.T) {
	m := newLeader(t, nil)
	for _, p := range []raft.NodeID{"n2", "n3", "n4", "n5"} {
		feed(m, p, t0, 5*time.Second, constant(10*time.Millisecond))
	}
	x, ok := quorumFeatures(m.peers, fiveNodes(), "n1", t0.Add(5*time.Second))
	if !ok {
		t.Fatal("no features")
	}
	if math.Abs(x[0]-10) > 0.01 || math.Abs(x[1]-10) > 0.01 || x[2] > 0.01 || x[5] != 0 {
		t.Fatalf("steady 10ms link gave %v", x)
	}
	if math.Abs(x[4]) > 0.01 {
		t.Fatalf("a flat RTT must have no slope, got %v", x[4])
	}
}

func TestSlopeAndJitterTrackGrowingDelay(t *testing.T) {
	m := newLeader(t, nil)
	for _, p := range []raft.NodeID{"n2", "n3", "n4", "n5"} {
		// Round trips grow by 100ms per second.
		feed(m, p, t0, 3*time.Second, func(at time.Time) time.Duration {
			return time.Duration(at.Sub(t0).Seconds() * 100 * float64(time.Millisecond))
		})
	}
	x, ok := quorumFeatures(m.peers, fiveNodes(), "n1", t0.Add(3*time.Second))
	if !ok {
		t.Fatal("no features")
	}
	if math.Abs(x[4]-100) > 1 {
		t.Fatalf("slope should be about 100ms/s, got %v", x[4])
	}
	if x[1] <= x[0] {
		t.Fatalf("recent RTT (%v) should exceed the window mean (%v) while delay is growing", x[1], x[0])
	}
	if x[2] < 10 {
		t.Fatalf("a changing RTT should show as jitter, got %v", x[2])
	}
}

// The heart of the feature design: a bad link to a minority of followers must not change
// the leader's reading at all, while trouble on the leader's own side must.
func TestQuorumViewIgnoresMinorityOfBadLinks(t *testing.T) {
	healthy := constant(5 * time.Millisecond)
	slow := constant(400 * time.Millisecond)
	silent := func(time.Time) time.Duration { return -1 }

	reading := func(rtts map[raft.NodeID]func(time.Time) time.Duration) []float64 {
		m := newLeader(t, nil)
		for p, r := range rtts {
			feed(m, p, t0, 4*time.Second, r)
		}
		x, ok := quorumFeatures(m.peers, fiveNodes(), "n1", t0.Add(4*time.Second))
		if !ok {
			t.Fatal("no features")
		}
		return x
	}

	all := reading(map[raft.NodeID]func(time.Time) time.Duration{"n2": healthy, "n3": healthy, "n4": healthy, "n5": healthy})
	one := reading(map[raft.NodeID]func(time.Time) time.Duration{"n2": healthy, "n3": healthy, "n4": healthy, "n5": slow})
	two := reading(map[raft.NodeID]func(time.Time) time.Duration{"n2": healthy, "n3": healthy, "n4": silent, "n5": slow})
	for k := range FeatureNames {
		if math.Abs(all[k]-one[k]) > 1e-9 || math.Abs(all[k]-two[k]) > 1e-9 {
			t.Fatalf("%s moved because of a minority of bad links: %v / %v / %v", FeatureNames[k], all[k], one[k], two[k])
		}
	}

	sick := reading(map[raft.NodeID]func(time.Time) time.Duration{"n2": slow, "n3": slow, "n4": slow, "n5": slow})
	if sick[0] < 300 {
		t.Fatalf("a slow leader must show in the reading: %v", sick)
	}
	// Losing a majority of followers shows up too.
	maj := reading(map[raft.NodeID]func(time.Time) time.Duration{"n2": healthy, "n3": silent, "n4": silent, "n5": silent})
	if maj[5] < 0.9 {
		t.Fatalf("a silent majority must show as missed heartbeats: %v", maj)
	}
}

func TestMissRatioCountsOnlyWhatHadTimeToArrive(t *testing.T) {
	var w peerWindow
	start := t0
	for i := 0; i < 40; i++ {
		at := start.Add(time.Duration(i) * 50 * time.Millisecond)
		w.sent(at)
		if i < 20 {
			w.acked(at, at.Add(5*time.Millisecond))
		}
	}
	now := start.Add(2 * time.Second)
	f, ok := w.peerFeatures(now)
	if !ok {
		t.Fatal("no features")
	}
	// 34 heartbeats are older than the grace period, of which 14 went unanswered.
	if f[5] < 0.35 || f[5] > 0.45 {
		t.Fatalf("miss ratio %v", f[5])
	}
}

func TestSilentPeerLooksDegradedNotHealthy(t *testing.T) {
	var w peerWindow
	for i := 0; i < 40; i++ {
		w.sent(t0.Add(time.Duration(i) * 50 * time.Millisecond))
	}
	f, ok := w.peerFeatures(t0.Add(2 * time.Second))
	if !ok {
		t.Fatal("silent peer should yield a reading")
	}
	if f[0] < 1000 || f[5] != 1 {
		t.Fatalf("no replies must not read as zero latency: %v", f)
	}
}

func treeModel() *Model {
	// if rtt_mean_ms <= 100 then 0.0 else 1.0
	return &Model{
		Kind: "decision_tree", Features: FeatureNames, Threshold: 0.5, Consecutive: 3,
		Trees: []Tree{{
			Left: []int{1, -1, -1}, Right: []int{2, -1, -1}, Feature: []int{0, -2, -2},
			Threshold: []float64{100, 0, 0}, Prob: []float64{0.5, 0, 1},
		}},
	}
}

func allPeers(m *Monitor, start time.Time, d time.Duration, rtt time.Duration) {
	for _, p := range []raft.NodeID{"n2", "n3", "n4", "n5"} {
		feed(m, p, start, d, constant(rtt))
	}
}

func status(last uint64) raft.Status {
	return raft.Status{
		Members: fiveNodes(), LastIndex: last,
		Match: map[raft.NodeID]uint64{"n1": last, "n2": last, "n3": last, "n4": last, "n5": last},
	}
}

func TestHandoverRequiresConsecutiveReadings(t *testing.T) {
	m := newLeader(t, treeModel())
	allPeers(m, t0, 6*time.Second, 300*time.Millisecond)
	now := t0.Add(6 * time.Second)
	if to := m.Evaluate(status(9), now); to != raft.None {
		t.Fatalf("one reading must not trigger a handover (%s)", to)
	}
	if to := m.Evaluate(status(9), now); to != raft.None {
		t.Fatalf("two readings must not trigger a handover (%s)", to)
	}
	if to := m.Evaluate(status(9), now); to == raft.None {
		t.Fatal("the third consecutive reading should trigger a handover")
	}
}

func TestBlipResetsTheCount(t *testing.T) {
	m := newLeader(t, treeModel())
	allPeers(m, t0, 6*time.Second, 300*time.Millisecond)
	now := t0.Add(6 * time.Second)
	m.Evaluate(status(9), now)
	m.Evaluate(status(9), now)
	// Recovery: replace the history with a healthy reading.
	m2 := newLeader(t, treeModel())
	allPeers(m2, t0, 6*time.Second, 5*time.Millisecond)
	m.peers = m2.peers
	m.Evaluate(status(9), now)
	if m.over != 0 {
		t.Fatal("a healthy reading must reset the count")
	}
}

func TestNoPredictionDuringWarmup(t *testing.T) {
	m := newLeader(t, treeModel())
	allPeers(m, t0, time.Second, 300*time.Millisecond)
	for i := 0; i < 10; i++ {
		if to := m.Evaluate(status(9), t0.Add(time.Second)); to != raft.None {
			t.Fatal("must not act on a new leader's first readings")
		}
	}
}

func TestCooldownLimitsHandovers(t *testing.T) {
	m := newLeader(t, treeModel())
	allPeers(m, t0, 6*time.Second, 300*time.Millisecond)
	now := t0.Add(6 * time.Second)
	got := 0
	for i := 0; i < 30; i++ {
		if m.Evaluate(status(9), now) != raft.None {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("%d handovers within one cooldown", got)
	}
}

func TestTargetIsHealthiestUpToDateFollower(t *testing.T) {
	m := newLeader(t, nil)
	feed(m, "n2", t0, 4*time.Second, constant(50*time.Millisecond))
	feed(m, "n3", t0, 4*time.Second, constant(5*time.Millisecond)) // fastest, but far behind
	feed(m, "n4", t0, 4*time.Second, constant(20*time.Millisecond))
	feed(m, "n5", t0, 4*time.Second, constant(30*time.Millisecond))
	st := status(1000)
	st.Match["n3"] = 10
	if got := m.chooseTarget(st, t0.Add(4*time.Second)); got != "n4" {
		t.Fatalf("expected n4, the fastest follower that is caught up, got %s", got)
	}
}

func TestLeadershipChangeClearsHistory(t *testing.T) {
	m := newLeader(t, treeModel())
	allPeers(m, t0, 6*time.Second, 300*time.Millisecond)
	now := t0.Add(6 * time.Second)
	m.Evaluate(status(9), now)
	m.LeadershipChanged(false, now)
	m.LeadershipChanged(true, now)
	if m.over != 0 || len(m.peers) != 0 {
		t.Fatal("history from a previous period of leadership must not carry over")
	}
}

func TestTelemetryIsDurableAndCarriesFeatures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.csv")
	m, err := NewMonitor(MonitorConfig{ID: "n1", TelemetryPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.LeadershipChanged(true, t0)
	allPeers(m, t0, 4*time.Second, 5*time.Millisecond)
	m.Evaluate(status(1), t0.Add(4*time.Second))
	// Read without closing: a killed process does not get to flush.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("telemetry was not written through")
	}
}

func TestEvaluatorsByHand(t *testing.T) {
	x := []float64{10, 20, 30, 40, 50, 0.5}
	lr := &Model{Kind: "logistic_regression", Coef: []float64{1, 0, 0, 0, 0, 0}, Mean: make([]float64, 6),
		Scale: []float64{1, 1, 1, 1, 1, 1}, Inter: -10}
	if p := lr.Probability(x); math.Abs(p-0.5) > 1e-12 {
		t.Fatalf("z=0 should give 0.5, got %v", p)
	}
	forest := &Model{Kind: "random_forest", Trees: []Tree{
		{Left: []int{-1}, Right: []int{-1}, Feature: []int{-2}, Threshold: []float64{0}, Prob: []float64{1}},
		{Left: []int{-1}, Right: []int{-1}, Feature: []int{-2}, Threshold: []float64{0}, Prob: []float64{0}},
	}}
	if p := forest.Probability(x); p != 0.5 {
		t.Fatalf("a forest should average its trees, got %v", p)
	}
	one := []float64{1, 1, 1, 1, 1, 1}
	nb := &Model{Kind: "naive_bayes", Prior: [2]float64{0.5, 0.5},
		Theta: [2][]float64{{0, 0, 0, 0, 0, 0}, {1, 1, 1, 1, 1, 1}}, Var: [2][]float64{one, one}}
	if p := nb.Probability([]float64{0.5, 0.5, 0.5, 0.5, 0.5, 0.5}); math.Abs(p-0.5) > 1e-12 {
		t.Fatalf("a point midway between identical classes should give 0.5, got %v", p)
	}
	if p := nb.Probability(one); p <= 0.5 {
		t.Fatalf("a point at the class 1 mean should favour class 1, got %v", p)
	}
	// Extreme values must not overflow into NaN and confuse the threshold comparison.
	huge := []float64{1e12, 1e12, 1e12, 1e12, 1e12, 1e12}
	if p := nb.Probability(huge); math.IsNaN(p) {
		t.Fatal("naive Bayes produced NaN")
	}
	if p := lr.Probability(huge); math.IsNaN(p) {
		t.Fatal("logistic regression produced NaN")
	}
}

func TestLoadRejectsMalformedModels(t *testing.T) {
	dir := t.TempDir()
	write := func(m any) string {
		p := filepath.Join(dir, "m.json")
		b, _ := json.Marshal(m)
		os.WriteFile(p, b, 0o644)
		return p
	}
	good := treeModel()
	if _, err := LoadModel(write(good)); err != nil {
		t.Fatalf("a valid model was rejected: %v", err)
	}
	bad := func(f func(*Model)) *Model {
		m := treeModel()
		m.Trees = []Tree{treeModel().Trees[0]}
		f(m)
		return m
	}
	cases := map[string]*Model{
		"wrong features": bad(func(m *Model) { m.Features = []string{"a"} }),
		"bad threshold":  bad(func(m *Model) { m.Threshold = 0 }),
		"no consecutive": bad(func(m *Model) { m.Consecutive = 0 }),
		"unknown kind":   bad(func(m *Model) { m.Kind = "neural_net" }),
		"self-loop":      bad(func(m *Model) { m.Trees[0].Left[0] = 0 }),
		"bad feature":    bad(func(m *Model) { m.Trees[0].Feature[0] = 99 }),
	}
	for name, m := range cases {
		if _, err := LoadModel(write(m)); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
}

// Trained models carry scikit-learn's own predictions; ours must match them exactly. It is
// skipped until training has produced models.
func TestExportedModelsMatchScikitLearn(t *testing.T) {
	paths, _ := filepath.Glob(filepath.Join("..", "..", "ml", "models", "*.json"))
	if len(paths) == 0 {
		t.Skip("no exported models yet; run ml/train.py")
	}
	for _, p := range paths {
		m, err := LoadModel(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if len(m.Golden) == 0 {
			t.Errorf("%s has no golden predictions", p)
		}
		for i, g := range m.Golden {
			if got := m.Probability(g.X); math.Abs(got-g.P) > 1e-9 {
				t.Errorf("%s input %d: got %v, scikit-learn says %v", p, i, got, g.P)
				break
			}
		}
	}
}
