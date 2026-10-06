package rules

// bandPoints pays the points of the highest threshold at or below value. Bands,
// not exact counts: a fourth source or colleague adds nothing rather than
// falling off a cliff to zero, which an exact-match lookup would do. ok is
// false when no threshold is at or below value (no points).
// A rubric threshold may be negative, so "none matched" is the ok flag, not a
// sentinel threshold.
func bandPoints(points map[float64]float64, value float64) (threshold, pts float64, ok bool) {
	for th, p := range points {
		if th <= value && (!ok || th > threshold) {
			threshold, pts, ok = th, p, true
		}
	}
	return threshold, pts, ok
}
