//go:build race

package indicator

// The race detector adds allocations, so allocation budgets do not apply.
const raceEnabled = true
