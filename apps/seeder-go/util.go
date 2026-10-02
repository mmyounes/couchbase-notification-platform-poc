package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func powInt(base, exp int) float64 {
	return math.Pow(float64(base), float64(exp))
}

// decodeUlidTime recovers the millisecond timestamp from a ULID prefix.
// Used by --dry-run to assert the ULID/timestamp invariant that the whole
// sort_key pagination design depends on (spec section 7.2).
func decodeUlidTime(u string) int64 {
	var ms int64
	for _, c := range u[:10] {
		ms = ms*32 + int64(strings.IndexRune(b32, c))
	}
	return ms
}

func parseIso(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return -1
	}
	return t.UnixMilli()
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
