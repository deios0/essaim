//go:build !race

package server

// raceEnabledServer is false in a normal (non-race) test build, so timing-budget
// assertions run and enforce the <15ms hot-path ceiling (P0-1).
const raceEnabledServer = false
