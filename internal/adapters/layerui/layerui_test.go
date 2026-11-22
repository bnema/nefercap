package layerui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// NeferWL sends the preferred scale before the first configure: the size is
// still unknown (zero for an anchored layer), so OnResize must wait.
func TestScaleBeforeConfigureIsNotAResize(t *testing.T) {
	calls := 0
	l := &loop[int]{cfg: Config{OnResize: func(int, int, float64) { calls++ }}}
	l.Scale(0, 1.5)
	assert.Zero(t, calls)
}
