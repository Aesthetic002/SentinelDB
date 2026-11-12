package predict

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// Model is a trained classifier exported by ml/train.py.
type Model struct {
	Kind     string   `json:"kind"` // decision_tree, random_forest, logistic_regression, naive_bayes
	Features []string `json:"features"`
	// Threshold is the probability at which a reading counts as "about to fail".
	Threshold float64 `json:"threshold"`
	// Consecutive is how many readings in a row must exceed it before acting.
	Consecutive int `json:"consecutive"`

	Trees []Tree       `json:"trees,omitempty"` // decision_tree, random_forest
	Mean  []float64    `json:"mean,omitempty"`  // logistic_regression: scaler
	Scale []float64    `json:"scale,omitempty"` // logistic_regression: scaler
	Coef  []float64    `json:"coef,omitempty"`  // logistic_regression
	Inter float64      `json:"intercept,omitempty"`
	Theta [2][]float64 `json:"theta,omitempty"` // naive_bayes: per-class means
	Var   [2][]float64 `json:"var,omitempty"`   // naive_bayes: per-class variances
	Prior [2]float64   `json:"prior,omitempty"` // naive_bayes

	// Golden holds inputs with scikit-learn's own predicted probability, so tests can
	// check this evaluator agrees with the library the model was trained in.
	Golden []Golden `json:"golden,omitempty"`
}

// Golden is one recorded input and its expected output.
type Golden struct {
	X []float64 `json:"x"`
	P float64   `json:"p"`
}

// Tree is one decision tree in scikit-learn's array form. A node is a leaf when Left is -1.
type Tree struct {
	Left      []int     `json:"left"`
	Right     []int     `json:"right"`
	Feature   []int     `json:"feature"`
	Threshold []float64 `json:"threshold"`
	// Prob is the fraction of positive training samples at each node.
	Prob []float64 `json:"prob"`
}

// LoadModel reads and validates an exported model.
func LoadModel(path string) (*Model, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Model
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

func (m *Model) validate() error {
	if len(m.Features) != len(FeatureNames) {
		return fmt.Errorf("model expects %d features, this build computes %d", len(m.Features), len(FeatureNames))
	}
	for i, f := range m.Features {
		if f != FeatureNames[i] {
			return fmt.Errorf("feature %d is %q in the model but %q in this build", i, f, FeatureNames[i])
		}
	}
	n := len(FeatureNames)
	switch m.Kind {
	case "decision_tree", "random_forest":
		if len(m.Trees) == 0 {
			return fmt.Errorf("%s model has no trees", m.Kind)
		}
		for ti, t := range m.Trees {
			size := len(t.Left)
			if size == 0 || len(t.Right) != size || len(t.Feature) != size || len(t.Threshold) != size || len(t.Prob) != size {
				return fmt.Errorf("tree %d has inconsistent arrays", ti)
			}
			for i := 0; i < size; i++ {
				if t.Left[i] == -1 {
					continue
				}
				// Children must lie strictly later, which also rules out a cycle that would
				// hang the consensus loop during evaluation.
				if t.Left[i] <= i || t.Left[i] >= size || t.Right[i] <= i || t.Right[i] >= size || t.Feature[i] < 0 || t.Feature[i] >= n {
					return fmt.Errorf("tree %d node %d is malformed", ti, i)
				}
			}
		}
	case "logistic_regression":
		if len(m.Coef) != n || len(m.Mean) != n || len(m.Scale) != n {
			return fmt.Errorf("logistic model has inconsistent arrays")
		}
	case "naive_bayes":
		for c := 0; c < 2; c++ {
			if len(m.Theta[c]) != n || len(m.Var[c]) != n {
				return fmt.Errorf("naive Bayes model has inconsistent arrays")
			}
		}
	default:
		return fmt.Errorf("unknown model kind %q", m.Kind)
	}
	if m.Threshold <= 0 || m.Threshold >= 1 {
		return fmt.Errorf("threshold %v is outside (0, 1)", m.Threshold)
	}
	if m.Consecutive < 1 {
		return fmt.Errorf("consecutive must be at least 1")
	}
	return nil
}

// Probability returns the estimated probability that this node is about to fail.
func (m *Model) Probability(x []float64) float64 {
	switch m.Kind {
	case "decision_tree", "random_forest":
		// A forest averages its trees' probabilities, as scikit-learn does.
		var sum float64
		for i := range m.Trees {
			sum += m.Trees[i].predict(x)
		}
		return sum / float64(len(m.Trees))
	case "logistic_regression":
		z := m.Inter
		for i, c := range m.Coef {
			z += c * (x[i] - m.Mean[i]) / m.Scale[i]
		}
		return 1 / (1 + math.Exp(-z))
	case "naive_bayes":
		// Work in logs: multiplying many small densities would underflow to zero.
		var ll [2]float64
		for c := 0; c < 2; c++ {
			ll[c] = math.Log(m.Prior[c])
			for i, v := range x {
				d := v - m.Theta[c][i]
				ll[c] -= 0.5 * (math.Log(2*math.Pi*m.Var[c][i]) + d*d/m.Var[c][i])
			}
		}
		// P(class 1) = e^l1 / (e^l0 + e^l1) = 1 / (1 + e^(l0-l1)).
		return 1 / (1 + math.Exp(ll[0]-ll[1]))
	}
	return 0
}

func (t *Tree) predict(x []float64) float64 {
	i := 0
	for t.Left[i] != -1 {
		if x[t.Feature[i]] <= t.Threshold[i] {
			i = t.Left[i]
		} else {
			i = t.Right[i]
		}
	}
	return t.Prob[i]
}
