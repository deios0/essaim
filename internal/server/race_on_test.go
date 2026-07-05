//go:build race

package server

// raceEnabledServer is true when the test binary is built with -race. The race
// detector inflates wall-clock timings ~10x, so timing-budget assertions are
// skipped under -race (correctness is still asserted).
const raceEnabledServer = true
