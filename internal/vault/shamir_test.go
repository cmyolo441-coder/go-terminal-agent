package vault

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
)

// gfMulRef is a straightforward reference multiplication.
func gfMulRef(a, b byte) byte {
	var p byte
	for b != 0 {
		if b&1 != 0 {
			p ^= a
		}
		carry := a&0x80 != 0
		a <<= 1
		if carry {
			a ^= 0x1b
		}
		b >>= 1
	}
	return p
}

func TestGFArithmeticExhaustive(t *testing.T) {
	for a := range 256 {
		for b := range 256 {
			if got, want := gfMul(byte(a), byte(b)), gfMulRef(byte(a), byte(b)); got != want {
				t.Fatalf("gfMul(%d,%d) = %d, want %d", a, b, got, want)
			}
		}
		inv := gfInv(byte(a))
		if a == 0 {
			if inv != 0 {
				t.Fatal("inv(0) != 0")
			}
			continue
		}
		if gfMul(byte(a), inv) != 1 {
			t.Fatalf("inv(%d) = %d is wrong", a, inv)
		}
	}
	// Known AES vector: {57} x {83} = {c1}.
	if gfMul(0x57, 0x83) != 0xc1 {
		t.Fatal("FIPS-197 multiplication vector failed")
	}
}

// combinations calls f with every k-subset of [0, n).
func combinations(n, k int, f func([]int)) {
	idx := make([]int, k)
	var rec func(start, depth int)
	rec = func(start, depth int) {
		if depth == k {
			f(idx)
			return
		}
		for i := start; i < n; i++ {
			idx[depth] = i
			rec(i+1, depth+1)
		}
	}
	rec(0, 0)
}

func TestShamirAnyKOfN(t *testing.T) {
	for _, tc := range []struct{ k, n int }{{2, 2}, {2, 3}, {3, 5}, {4, 7}, {5, 5}} {
		secret := make([]byte, 32)
		_, _ = rand.Read(secret)
		shares, err := SplitSecret(secret, tc.k, tc.n)
		if err != nil {
			t.Fatal(err)
		}
		// Every k-subset and every larger subset reconstructs.
		for size := tc.k; size <= tc.n; size++ {
			combinations(tc.n, size, func(idx []int) {
				sub := make([]Share, len(idx))
				for i, j := range idx {
					sub[i] = shares[j]
				}
				got, err := CombineShares(sub)
				if err != nil || !bytes.Equal(got, secret) {
					t.Fatalf("k=%d n=%d subset %v: %v", tc.k, tc.n, idx, err)
				}
			})
		}
		// Every (k-1)-subset is refused.
		combinations(tc.n, tc.k-1, func(idx []int) {
			sub := make([]Share, len(idx))
			for i, j := range idx {
				sub[i] = shares[j]
			}
			if _, err := CombineShares(sub); !errors.Is(err, ErrNotEnoughShares) {
				t.Fatalf("k-1 shares: %v", err)
			}
		})
	}
}

func TestShamirIntegrity(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	shares, err := SplitSecret(secret, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := SplitSecret(secret, 3, 5)

	// Modified y value is caught by the commitment.
	bad := shares[0]
	bad.Y = bytes.Clone(bad.Y)
	bad.Y[3] ^= 1
	_, err = CombineShares([]Share{bad, shares[1], shares[2]})
	wantErr(t, err, ErrShareIntegrity)

	// A corrupted extra share is caught by the consistency check.
	extra := shares[4]
	extra.Y = bytes.Clone(extra.Y)
	extra.Y[0] ^= 0x80
	_, err = CombineShares([]Share{shares[0], shares[1], shares[2], extra})
	wantErr(t, err, ErrShareIntegrity)

	// Shares from different sets cannot be mixed.
	_, err = CombineShares([]Share{shares[0], shares[1], other[2]})
	wantErr(t, err, ErrShareIntegrity)

	// Duplicates do not count twice; conflicting duplicates are rejected.
	_, err = CombineShares([]Share{shares[0], shares[0], shares[1]})
	wantErr(t, err, ErrNotEnoughShares)
	conflict := shares[0]
	conflict.Y = bytes.Clone(conflict.Y)
	conflict.Y[0]++
	_, err = CombineShares([]Share{shares[0], conflict, shares[1], shares[2]})
	wantErr(t, err, ErrShareIntegrity)

	// Binary checksum catches any single-byte change.
	enc, err := shares[1].MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	for i := range enc {
		mod := bytes.Clone(enc)
		mod[i] ^= 0x04
		if _, err := ParseShare(mod); !errors.Is(err, ErrShareIntegrity) {
			t.Fatalf("byte %d flip: %v", i, err)
		}
	}
	dec, err := ParseShare(enc)
	if err != nil || dec.X != shares[1].X || !bytes.Equal(dec.Y, shares[1].Y) {
		t.Fatalf("parse: %v", err)
	}
	if s := fmt.Sprint(shares[1]); bytes.Contains([]byte(s), []byte(fmt.Sprintf("%x", shares[1].Y))) {
		t.Fatal("String leaks share value")
	}
}

func TestShamirArgs(t *testing.T) {
	for _, tc := range []struct{ k, n, l int }{{1, 3, 8}, {4, 3, 8}, {2, 256, 8}, {2, 3, 0}, {2, 3, MaxShareSecretSize + 1}} {
		if _, err := SplitSecret(make([]byte, tc.l), tc.k, tc.n); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	shares, err := SplitSecret([]byte{42}, 2, 255)
	if err != nil || len(shares) != 255 || shares[254].X != 255 {
		t.Fatalf("n=255: %v", err)
	}
	got, err := CombineShares([]Share{shares[200], shares[7]})
	if err != nil || got[0] != 42 {
		t.Fatalf("combine: %v", err)
	}
}

// chiSquare256 returns the chi-square statistic of byte counts against uniform.
func chiSquare256(counts *[256]int, total int) float64 {
	exp := float64(total) / 256
	var x float64
	for _, c := range counts {
		d := float64(c) - exp
		x += d * d / exp
	}
	return x
}

// TestShamirKMinusOneRevealsNothing checks statistically that, for k = 3,
// any two shares are uniformly distributed regardless of the secret: the
// marginals of y1 and y2 and their difference y1^y2 are uniform for an
// all-zero and an all-0xFF secret. With 255 degrees of freedom the chi-square
// statistic exceeds 400 with probability below 1e-8.
func TestShamirKMinusOneRevealsNothing(t *testing.T) {
	const (
		trials   = 2048
		size     = 64
		maxChi2  = 400.0
		samples  = trials * size
		k, n     = 3, 5
		xA, xB   = 1, 4 // share indexes 0-based: x = 2 and x = 5
		secretFF = 0xff
	)
	for _, fill := range []byte{0x00, secretFF} {
		secret := bytes.Repeat([]byte{fill}, size)
		var ca, cb, cx [256]int
		for range trials {
			shares, err := SplitSecret(secret, k, n)
			if err != nil {
				t.Fatal(err)
			}
			for i := range size {
				ya, yb := shares[xA].Y[i], shares[xB].Y[i]
				ca[ya]++
				cb[yb]++
				cx[ya^yb]++
			}
		}
		for name, c := range map[string]*[256]int{"y_a": &ca, "y_b": &cb, "y_a^y_b": &cx} {
			if x := chiSquare256(c, samples); x > maxChi2 {
				t.Errorf("secret %#x: %s chi2 = %.1f > %.0f", fill, name, x, maxChi2)
			}
		}
	}
}

// TestShamirPerfectSecrecyExhaustive verifies the information-theoretic
// argument directly for k = 2: one share (x, y) is consistent with every
// possible secret byte via exactly one degree-1 polynomial.
func TestShamirPerfectSecrecyExhaustive(t *testing.T) {
	for x := 1; x < 256; x++ {
		for _, y := range []byte{0, 1, 0x80, 0xff} {
			seen := make(map[byte]bool)
			for s := range 256 {
				// a1 = (y - s) / x
				a1 := gfMul(y^byte(s), gfInv(byte(x)))
				if gfMul(a1, byte(x))^byte(s) != y {
					t.Fatalf("x=%d y=%d s=%d inconsistent", x, y, s)
				}
				if seen[a1] {
					t.Fatalf("x=%d y=%d: coefficient reused", x, y)
				}
				seen[a1] = true
			}
		}
	}
}
