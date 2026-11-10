//go:build race

package web

// The race detector slows queries down several times over.
const raceEnabled = true
