//go:build race

package wayland

// The race detector adds allocations, so allocation budgets do not apply.
const raceEnabled = true
