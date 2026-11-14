package sentinel

import "math/bits"

const (
	fnvOffset = 14695981039346656037
	fnvPrime  = 1099511628211
)

// fnv64 is FNV-1a over s.
func fnv64(s string) uint64 {
	h := uint64(fnvOffset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	return h
}

// fnv64b is FNV-1a over b.
func fnv64b(b []byte) uint64 {
	h := uint64(fnvOffset)
	for _, c := range b {
		h ^= uint64(c)
		h *= fnvPrime
	}
	return h
}

// fnvAdd folds s and a separator into h.
func fnvAdd(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	h ^= 0xff
	h *= fnvPrime
	return h
}

// fnvAddU64 folds v into h.
func fnvAddU64(h, v uint64) uint64 {
	for i := 0; i < 8; i++ {
		h ^= v & 0xff
		h *= fnvPrime
		v >>= 8
	}
	return h
}

// mix64 is the SplitMix64 finaliser; it spreads FNV's weak low bits so that
// every SimHash bit is close to a fair coin.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// lowerInto writes an ASCII-lowercased copy of s into dst (reusing its
// storage) with backslashes turned into slashes, so Windows paths match the
// same rules. Byte offsets are preserved: lower[i:j] corresponds to s[i:j].
func lowerInto(dst []byte, s string) []byte {
	dst = append(dst[:0], s...)
	for i, c := range dst {
		switch {
		case 'A' <= c && c <= 'Z':
			dst[i] = c + ('a' - 'A')
		case c == '\\':
			dst[i] = '/'
		}
	}
	return dst
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func isWordByte(c byte) bool {
	return 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_' || 'A' <= c && c <= 'Z' || c >= 0x80
}

// fingerprint hashes the tool name and the lowered target after collapsing
// whitespace and dropping quote characters, so `ls  "a"` and `ls a` are the
// same call.
func fingerprint(tool string, lower []byte) uint64 {
	h := fnvAdd(fnvOffset, tool)
	pendingSpace := false
	started := false
	for _, c := range lower {
		if isSpace(c) {
			pendingSpace = started
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			continue
		}
		if pendingSpace {
			h ^= ' '
			h *= fnvPrime
			pendingSpace = false
		}
		started = true
		h ^= uint64(c)
		h *= fnvPrime
	}
	return h
}

// simhash computes a 64-bit SimHash over the word tokens (unigrams) and
// adjacent token pairs (bigrams) of the lowered target plus the tool name,
// and returns the number of unigram tokens. Bigrams make a changed token
// weigh three features rather than one, which separates "same command,
// different argument" from "same command, one extra flag". Similar inputs
// have hashes a small Hamming distance apart.
func simhash(tool string, lower []byte) (uint64, int) {
	// Bit-sliced accumulation: lane j of acc[k] (16 bits each) counts how
	// many features had bit 4k+j set, so a feature costs 16 additions
	// instead of 64. maxFeatures keeps the lanes from overflowing.
	const maxFeatures = 1 << 14
	var acc [16]uint64
	features := 0
	add := func(h uint64) {
		if features == maxFeatures {
			return
		}
		features++
		h = mix64(h)
		for k := 0; k < 16; k++ {
			acc[k] += nibbleSpread[h>>(4*k)&0xf]
		}
	}
	prev := fnv64(tool)
	add(prev)
	n := 1
	start := -1
	for i := 0; i <= len(lower); i++ {
		if i < len(lower) && isWordByte(lower[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			h := fnv64b(lower[start:i])
			add(h)
			add(prev*0x9e3779b97f4a7c15 ^ h)
			prev = h
			n++
			start = -1
		}
	}
	var out uint64
	for k := 0; k < 16; k++ {
		for j := 0; j < 4; j++ {
			// Majority vote: more features had the bit set than clear.
			if 2*int(acc[k]>>(16*j)&0xffff) > features {
				out |= 1 << (4*k + j)
			}
		}
	}
	return out, n
}

// nibbleSpread maps a 4-bit value to four 16-bit lanes holding its bits.
var nibbleSpread = func() (t [16]uint64) {
	for v := range t {
		for j := 0; j < 4; j++ {
			if v>>j&1 == 1 {
				t[v] |= 1 << (16 * j)
			}
		}
	}
	return t
}()

func hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }
