//go:build race

package api

// raceEnabled reports whether the race detector is on: it multiplies every
// latency by an order of magnitude, so the budget test is skipped under it.
const raceEnabled = true
