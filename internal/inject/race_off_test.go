//go:build !race

package inject

// raceEnabled is false in a normal (non-race) test build, so timing-budget
// assertions run and enforce the <15ms hot-path ceiling.
const raceEnabled = false
