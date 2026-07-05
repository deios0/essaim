//go:build race

package inject

// raceEnabled is true when the test binary is built with -race. The race
// detector inflates wall-clock timings ~10x, so timing-budget assertions are
// skipped under -race (the zero-Marshal CORRECTNESS is still asserted).
const raceEnabled = true
