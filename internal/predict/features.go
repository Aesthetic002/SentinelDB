// Package predict is the prediction layer: it turns heartbeat timing into features,
// evaluates a trained classifier, and asks for a leadership transfer when a leader looks
// likely to fail.
//
// Models are trained offline in Python (ml/train.py) and exported to JSON. Evaluation here
// is plain arithmetic in Go, so there is no Python in the running system, and a test checks
// that the predictions match scikit-learn's on recorded inputs.
package predict

import (
	"math"
	"sort"
	"time"

	"sentineldb/internal/raft"
)

// FeatureNames lists the features in the order models expect. Everything here comes from
// messages the protocol already exchanges, so no extra instrumentation is needed.
var FeatureNames = []string{
	"rtt_mean_ms",    // how long responses are taking over the window
	"rtt_recent_ms",  // the same over only the last 500ms: where things are heading
	"rtt_jitter_ms",  // how irregular: a steady slow link is not an unreliable one
	"rtt_max_ms",     // the worst case in the window
	"rtt_slope_ms_s", // how fast round trips are growing, by linear regression
	"miss_ratio",     // the fraction of expected replies that never came
}

const (
	// Window is how much history each reading covers.
	Window = 2 * time.Second
	// missGrace is how long a heartbeat gets to be answered before it counts as missed.
	missGrace = 300 * time.Millisecond
	// minSamples is how many replies a peer needs before its features mean anything.
	minSamples = 5
	// ringSize comfortably holds one Window at the default 50ms heartbeat interval.
	ringSize = 256
)

type sample struct {
	sent  time.Time
	rtt   time.Duration
	acked bool
}

// peerWindow is a ring of recent heartbeats to one peer.
type peerWindow struct {
	buf   [ringSize]sample
	head  int
	count int
}

func (w *peerWindow) sent(at time.Time) {
	w.buf[w.head] = sample{sent: at}
	w.head = (w.head + 1) % ringSize
	if w.count < ringSize {
		w.count++
	}
}

func (w *peerWindow) acked(sent, now time.Time) {
	// Replies arrive close to in order, so the match is found near the head.
	for i := 1; i <= w.count; i++ {
		s := &w.buf[(w.head-i+ringSize)%ringSize]
		if s.sent.Equal(sent) {
			if !s.acked {
				s.acked = true
				s.rtt = now.Sub(sent)
			}
			return
		}
		if s.sent.Before(sent) {
			return
		}
	}
}

// peerFeatures summarizes one peer. ok is false if there is too little data so far.
func (w *peerWindow) peerFeatures(now time.Time) (f [6]float64, ok bool) {
	var ts, rs []float64
	var recentSum float64
	var recentN, expected, missed int
	from := now.Add(-Window)
	for i := 1; i <= w.count; i++ {
		s := &w.buf[(w.head-i+ringSize)%ringSize]
		if s.sent.Before(from) {
			break
		}
		if now.Sub(s.sent) >= missGrace {
			expected++
			if !s.acked {
				missed++
			}
		}
		if s.acked {
			ms := float64(s.rtt) / float64(time.Millisecond)
			ts = append(ts, s.sent.Sub(from).Seconds())
			rs = append(rs, ms)
			if now.Sub(s.sent) <= 500*time.Millisecond {
				recentSum += ms
				recentN++
			}
		}
	}
	if expected < minSamples {
		return f, false
	}
	missRatio := float64(missed) / float64(expected)
	if len(rs) < minSamples {
		// Silence is the most degraded a link can be. Report it as a full window of delay
		// rather than as an absence of data, which would look perfectly healthy.
		cap := float64(Window) / float64(time.Millisecond)
		return [6]float64{cap, cap, 0, cap, 0, missRatio}, true
	}
	var sum, maxV float64
	for _, r := range rs {
		sum += r
		maxV = math.Max(maxV, r)
	}
	mean := sum / float64(len(rs))
	var variance float64
	for _, r := range rs {
		variance += (r - mean) * (r - mean)
	}
	jitter := math.Sqrt(variance / float64(len(rs)))
	recent := mean
	if recentN > 0 {
		recent = recentSum / float64(recentN)
	}
	return [6]float64{mean, recent, jitter, maxV, slope(ts, rs), missRatio}, true
}

// slope is the least-squares gradient of ys against xs.
func slope(xs, ys []float64) float64 {
	n := float64(len(xs))
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / d
}

// quorumFeatures combines per-peer readings into one vector. For each feature it takes
// the value at the position the leader's quorum depends on: a 5-node leader needs replies
// from 2 of its 4 followers, so it reads the second-best follower. So a bad link to one or
// two followers does not change the reading at all, while a problem on the leader's own
// side affects every follower and does. This is what separates "the leader is sick" from
// "a follower is sick" without needing to label them separately.
func quorumFeatures(peers map[raft.NodeID]*peerWindow, members raft.Membership, self raft.NodeID, now time.Time) ([]float64, bool) {
	var readings [][6]float64
	for id := range members {
		if id == self {
			continue
		}
		w := peers[id]
		if w == nil {
			continue
		}
		if f, ok := w.peerFeatures(now); ok {
			readings = append(readings, f)
		}
	}
	needed := len(members) / 2 // followers needed for a majority including the leader
	if needed == 0 || len(readings) < needed {
		return nil, false
	}
	out := make([]float64, len(FeatureNames))
	vals := make([]float64, len(readings))
	for k := range FeatureNames {
		for i, r := range readings {
			vals[i] = r[k]
		}
		sort.Float64s(vals)
		out[k] = vals[needed-1]
	}
	return out, true
}
