package memory

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
)

// Embedder turns texts into dense vectors.
type Embedder interface {
	// Embed returns one vector per text, in order. Vectors need not be
	// normalised; the store normalises them.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dim is the vector dimension, or 0 when it is only known after the
	// first successful call (the store probes it once at open).
	Dim() int
	// ID names the model and every parameter that changes the vectors.
	// Vectors from embedders with different IDs are never mixed: opening a
	// store with a different embedder re-embeds every record.
	ID() string
}

// HashEmbedderConfig tunes a HashEmbedder. Zero fields take defaults.
type HashEmbedderConfig struct {
	// Dim is the vector dimension (default 1024; 64..8192).
	Dim int
	// Weights of the three feature families. Each family is L2-normalised
	// on its own before weighting, so the balance does not depend on the
	// length of the text.
	WordWeight   float64 // stemmed content-word unigrams (default 1.0)
	BigramWeight float64 // adjacent content-word bigrams (default 0.45)
	CharWeight   float64 // character 3..5-grams of each word (default 0.65)
	// MinGram and MaxGram bound the character n-gram lengths (3 and 5).
	MinGram, MaxGram int
}

// HashEmbedder is an offline embedder based on the hashing trick: word
// unigrams, word bigrams and character n-grams are hashed with a sign bit
// into a fixed number of dimensions, weighted by sublinear term frequency
// (1 + ln tf) and L2-normalised. It captures lexical and morphological
// similarity (typos, inflections, identifier fragments) with no model and
// no network, is deterministic across processes and platforms, and costs a
// few microseconds per text.
type HashEmbedder struct {
	cfg  HashEmbedderConfig
	id   string
	pool sync.Pool // *hashScratch
}

type hashScratch struct {
	counts [3]map[uint64]uint32
	acc    [3][]float32
	keys   []uint64
}

// NewHashEmbedder returns a HashEmbedder with cfg (defaults filled in).
func NewHashEmbedder(cfg HashEmbedderConfig) (*HashEmbedder, error) {
	if cfg.Dim == 0 {
		cfg.Dim = 1024
	}
	if cfg.Dim < 64 || cfg.Dim > 8192 {
		return nil, fmt.Errorf("memory: hash embedder dim %d outside [64, 8192]", cfg.Dim)
	}
	if cfg.WordWeight == 0 && cfg.BigramWeight == 0 && cfg.CharWeight == 0 {
		cfg.WordWeight, cfg.BigramWeight, cfg.CharWeight = 1, 0.45, 0.65
	}
	if cfg.WordWeight < 0 || cfg.BigramWeight < 0 || cfg.CharWeight < 0 {
		return nil, fmt.Errorf("memory: hash embedder weights must be non-negative")
	}
	if cfg.MinGram == 0 {
		cfg.MinGram = 3
	}
	if cfg.MaxGram == 0 {
		cfg.MaxGram = 5
	}
	if cfg.MinGram < 1 || cfg.MaxGram < cfg.MinGram || cfg.MaxGram > 8 {
		return nil, fmt.Errorf("memory: bad character n-gram range %d..%d", cfg.MinGram, cfg.MaxGram)
	}
	e := &HashEmbedder{cfg: cfg}
	e.id = fmt.Sprintf("hash-v1/d%d/w%g-%g-%g/c%d-%d", cfg.Dim, cfg.WordWeight, cfg.BigramWeight, cfg.CharWeight, cfg.MinGram, cfg.MaxGram)
	e.pool.New = func() any {
		s := &hashScratch{}
		for i := range s.counts {
			s.counts[i] = make(map[uint64]uint32, 64)
			s.acc[i] = make([]float32, cfg.Dim)
		}
		return s
	}
	return e, nil
}

// DefaultHashEmbedder returns a HashEmbedder with default settings.
func DefaultHashEmbedder() *HashEmbedder {
	e, err := NewHashEmbedder(HashEmbedderConfig{})
	if err != nil {
		panic(err)
	}
	return e
}

// Dim implements Embedder.
func (e *HashEmbedder) Dim() int { return e.cfg.Dim }

// ID implements Embedder.
func (e *HashEmbedder) ID() string { return e.id }

// Embed implements Embedder. It never fails except on cancellation.
func (e *HashEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		out[i] = e.EmbedOne(t)
	}
	return out, nil
}

// Feature-family seeds keep identical strings in different families apart.
const (
	seedWord   = 0x9e3779b97f4a7c15
	seedBigram = 0xc2b2ae3d27d4eb4f
	seedChar   = 0x165667b19e3779f9
)

// EmbedOne embeds a single text. The result is unit length, or all zeros
// for a text without any word.
func (e *HashEmbedder) EmbedOne(text string) []float32 {
	s := e.pool.Get().(*hashScratch)
	defer e.pool.Put(s)
	for i := range s.counts {
		clear(s.counts[i])
	}
	toks := tokenize(text)
	var prev uint64
	havePrev := false
	var runes []rune
	for _, t := range toks {
		if t.Stop {
			havePrev = false
			continue
		}
		hw := hashString(seedWord, t.Term)
		s.counts[0][hw]++
		if havePrev {
			s.counts[1][mix64(prev*0x100000001b3^hw^seedBigram)]++
		}
		prev, havePrev = hw, true
		// Character n-grams of "<raw>" capture morphology and typos.
		runes = append(runes[:0], '<')
		runes = append(runes, []rune(t.Raw)...)
		runes = append(runes, '>')
		for n := e.cfg.MinGram; n <= e.cfg.MaxGram; n++ {
			for i := 0; i+n <= len(runes); i++ {
				s.counts[2][hashRunes(seedChar+uint64(n), runes[i:i+n])]++
			}
		}
	}
	dim := uint64(e.cfg.Dim)
	weights := [3]float64{e.cfg.WordWeight, e.cfg.BigramWeight, e.cfg.CharWeight}
	out := make([]float32, e.cfg.Dim)
	for g := range s.counts {
		if weights[g] == 0 || len(s.counts[g]) == 0 {
			continue
		}
		acc := s.acc[g]
		clear(acc)
		// Visit features in a fixed order so colliding features are summed
		// identically every time: vectors are bit-for-bit reproducible.
		s.keys = s.keys[:0]
		for h := range s.counts[g] {
			s.keys = append(s.keys, h)
		}
		slices.Sort(s.keys)
		for _, h := range s.keys {
			w := float32(1 + math.Log(float64(s.counts[g][h])))
			if h>>63 != 0 {
				w = -w
			}
			acc[(h>>1)%dim] += w
		}
		var norm float64
		for _, x := range acc {
			norm += float64(x) * float64(x)
		}
		if norm == 0 {
			continue
		}
		scale := float32(weights[g] / math.Sqrt(norm))
		for i, x := range acc {
			out[i] += x * scale
		}
	}
	normalize(out)
	return out
}

// hashString is FNV-1a followed by a 64-bit finaliser; it is stable across
// processes and platforms, which persisted vectors require.
func hashString(seed uint64, s string) uint64 {
	h := uint64(0xcbf29ce484222325) ^ seed
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 0x100000001b3
	}
	return mix64(h)
}

func hashRunes(seed uint64, rs []rune) uint64 {
	h := uint64(0xcbf29ce484222325) ^ seed
	for _, r := range rs {
		h ^= uint64(r)
		h *= 0x100000001b3
	}
	return mix64(h)
}

// mix64 is the SplitMix64 finaliser.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
