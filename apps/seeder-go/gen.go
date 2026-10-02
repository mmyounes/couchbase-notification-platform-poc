package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Deterministic primitives. Every document is a pure function of
// (seed, event index), so a given --seed reproduces byte-identical documents.

// --- PRNG (mulberry32) ------------------------------------------------------
// Same algorithm as the reference implementation so determinism claims hold
// across both. uint32 wraparound matches JavaScript's Math.imul semantics.

type rng struct{ a uint32 }

func newRng(seed uint32) *rng { return &rng{a: seed} }

func (r *rng) next() float64 {
	r.a += 0x6D2B79F5
	a := r.a
	t1 := (a ^ (a >> 15)) * (1 | a)
	t2 := (t1 + ((t1 ^ (t1 >> 7)) * (61 | t1))) ^ t1
	return float64(t2^(t2>>14)) / 4294967296.0
}

func (r *rng) intn(n int) int { return int(r.next() * float64(n)) }

func (r *rng) pick(xs []string) string { return xs[r.intn(len(xs))] }

func (r *rng) pickInt(xs []int) int { return xs[r.intn(len(xs))] }

// seedFor gives a stable per-event seed so workers need no shared state.
func seedFor(seed uint32, i int) uint32 {
	return (seed ^ uint32(i)) * 2654435761
}

// --- Crockford base32 / ULID ------------------------------------------------
// event_id is a ULID: unique, lexicographically sortable, and time-ordered with
// the timestamp recoverable from the prefix. That last property is what lets a
// time-window filter become a range scan on sort_key (spec section 7.2), and it
// is why seeded documents MUST carry a ULID matching their timestamp field.

const b32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func encodeTime(ms int64, length int) string {
	out := make([]byte, length)
	for i := length - 1; i >= 0; i-- {
		out[i] = b32[ms%32]
		ms /= 32
	}
	return string(out)
}

func encodeRandom(r *rng, length int) string {
	out := make([]byte, length)
	for i := 0; i < length; i++ {
		out[i] = b32[r.intn(32)]
	}
	return string(out)
}

// ulidAt returns a ULID whose embedded timestamp is exactly ms.
func ulidAt(ms int64, r *rng) string { return encodeTime(ms, 10) + encodeRandom(r, 16) }

// UlidFloor / UlidCeil convert a time bound into a sort_key range bound.
func UlidFloor(ms int64) string { return encodeTime(ms, 10) + strings.Repeat("0", 16) }
func UlidCeil(ms int64) string  { return encodeTime(ms, 10) + strings.Repeat("Z", 16) }

// --- Zipf -------------------------------------------------------------------
// Real notification traffic is not uniform: a minority of users receive most
// messages. A uniform draw would make every cache and index look better
// behaved than production.

type zipfSampler struct{ cdf []float64 }

func newZipf(n int, s float64) *zipfSampler {
	cdf := make([]float64, n)
	acc := 0.0
	for i := 0; i < n; i++ {
		acc += 1.0 / math.Pow(float64(i+1), s)
		cdf[i] = acc
	}
	for i := range cdf {
		cdf[i] /= acc
	}
	return &zipfSampler{cdf: cdf}
}

// rank returns a 1-based rank.
func (z *zipfSampler) rank(r *rng) int {
	v := r.next()
	i := sort.SearchFloat64s(z.cdf, v)
	if i >= len(z.cdf) {
		i = len(z.cdf) - 1
	}
	return i + 1
}

// --- template rendering -----------------------------------------------------

var paramRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

// render substitutes {{param}}. A missing parameter is an error, not a blank.
func render(tpl string, params map[string]string) (string, error) {
	var missing string
	out := paramRe.ReplaceAllStringFunc(tpl, func(m string) string {
		k := m[2 : len(m)-2]
		v, ok := params[k]
		if !ok {
			missing = k
			return m
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("template parameter %q not supplied", missing)
	}
	return out, nil
}

// --- time formatting --------------------------------------------------------

func isoSec(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05Z")
}

func isoMilli(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func randDigits(r *rng, n int) string {
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = byte('0' + r.intn(10))
	}
	return string(out)
}

const tokenAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

func randToken(r *rng, n int) string {
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = tokenAlphabet[r.intn(len(tokenAlphabet))]
	}
	return string(out)
}
