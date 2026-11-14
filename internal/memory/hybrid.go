package memory

import (
	"math"
	"slices"
	"time"
)

// RankedList is one ranking to fuse: Keys in rank order (best first) and
// the list's weight (0 means 1).
type RankedList struct {
	Keys   []uint64
	Weight float64
}

// Fused is one result of FuseRRF. Ranks[i] is the 1-based rank of Key in
// list i, or 0 when the list does not contain it.
type Fused struct {
	Key   uint64
	Score float64
	Ranks []int
}

// FuseRRF combines rankings with Reciprocal Rank Fusion (Cormack, Clarke &
// Buettcher, 2009): score(d) = sum_i w_i / (k + rank_i(d)). k dampens the
// advantage of the very top ranks (60 in the paper; 0 or less selects it).
// Results are ordered by descending score, ties broken by best single rank
// and then key, so the output is deterministic.
func FuseRRF(k float64, lists ...RankedList) []Fused {
	if k <= 0 {
		k = 60
	}
	idx := map[uint64]int{}
	var out []Fused
	for li, l := range lists {
		w := l.Weight
		if w == 0 {
			w = 1
		}
		for r, key := range l.Keys {
			i, ok := idx[key]
			if !ok {
				i = len(out)
				idx[key] = i
				out = append(out, Fused{Key: key, Ranks: make([]int, len(lists))})
			}
			if out[i].Ranks[li] != 0 {
				continue // a key listed twice keeps its best rank
			}
			out[i].Ranks[li] = r + 1
			out[i].Score += w / (k + float64(r+1))
		}
	}
	best := func(f Fused) int {
		b := math.MaxInt
		for _, r := range f.Ranks {
			if r > 0 && r < b {
				b = r
			}
		}
		return b
	}
	slices.SortFunc(out, func(a, b Fused) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		if ba, bb := best(a), best(b); ba != bb {
			return ba - bb
		}
		switch {
		case a.Key < b.Key:
			return -1
		case a.Key > b.Key:
			return 1
		}
		return 0
	})
	return out
}

// MMR re-orders candidates with Maximal Marginal Relevance (Carbonell &
// Goldstein, 1998): it repeatedly picks the candidate maximising
//
//	lambda * relevance - (1 - lambda) * max similarity to the picks so far
//
// and returns up to k candidate indexes in pick order together with the
// redundancy penalty, (1-lambda) * max similarity, each pick paid.
// relevance should be in [0, 1]; sim(i, j) is a similarity in [-1, 1].
func MMR(relevance []float64, k int, lambda float64, sim func(i, j int) float64) (order []int, penalty []float64) {
	n := len(relevance)
	if k > n {
		k = n
	}
	if k <= 0 {
		return nil, nil
	}
	lambda = math.Max(0, math.Min(1, lambda))
	maxSim := make([]float64, n)
	for i := range maxSim {
		maxSim[i] = math.Inf(-1)
	}
	picked := make([]bool, n)
	for len(order) < k {
		bi, bs := -1, math.Inf(-1)
		for i := 0; i < n; i++ {
			if picked[i] {
				continue
			}
			red := 0.0
			if len(order) > 0 {
				red = math.Max(0, maxSim[i])
			}
			s := lambda*relevance[i] - (1-lambda)*red
			if s > bs {
				bi, bs = i, s
			}
		}
		picked[bi] = true
		red := 0.0
		if len(order) > 0 {
			red = (1 - lambda) * math.Max(0, maxSim[bi])
		}
		order = append(order, bi)
		penalty = append(penalty, red)
		if lambda == 1 {
			continue
		}
		for i := 0; i < n; i++ {
			if !picked[i] {
				if s := sim(i, bi); s > maxSim[i] {
					maxSim[i] = s
				}
			}
		}
	}
	return order, penalty
}

// RecencyWeights configures time decay per kind.
type RecencyWeights struct {
	// HalfLife per kind: after one half-life without access a memory's
	// recency factor drops halfway towards Floor. Zero entries use the
	// defaults (fact 90d, preference 365d, decision 180d, episode 21d).
	HalfLife map[Kind]time.Duration
	// Floor is the minimum recency factor (default 0.35), so old but
	// relevant memories are demoted, never hidden.
	Floor float64
}

var defaultHalfLife = map[Kind]time.Duration{
	KindFact:       90 * 24 * time.Hour,
	KindPreference: 365 * 24 * time.Hour,
	KindDecision:   180 * 24 * time.Hour,
	KindEpisode:    21 * 24 * time.Hour,
}

func (w RecencyWeights) halfLife(k Kind) time.Duration {
	if d := w.HalfLife[k]; d > 0 {
		return d
	}
	return defaultHalfLife[k]
}

// factor returns the recency multiplier in [Floor, 1] for a memory last
// touched age ago.
func (w RecencyWeights) factor(k Kind, age time.Duration) float64 {
	if age <= 0 {
		return 1
	}
	floor := w.Floor
	if floor <= 0 || floor > 1 {
		floor = 0.35
	}
	decay := math.Exp2(-age.Hours() / w.halfLife(k).Hours())
	return floor + (1-floor)*decay
}
