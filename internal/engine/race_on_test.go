//go:build race

package engine

// raceOn: the race detector slows every run several times over, so timing
// bounds do not hold under it.
const raceOn = true
