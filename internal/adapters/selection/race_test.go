//go:build race

package selection

// The race detector adds allocations, so allocation budgets do not apply.
const raceEnabled = true
