package capability

import (
	"testing"

	"github.com/skkap/yad/internal/seamtest"
)

// ListModelsForTests answers for every harness. A shipped yad that set it
// would report models no harness named, and every capability test would still
// pass, because each of them sets it too. The check is the source: any shipped
// file of the module that writes it, this package's own included.
func TestOnlyTestsReachTheModelLister(t *testing.T) {
	for _, at := range seamtest.Writes(t, "../..", "ListModelsForTests") {
		t.Errorf("%s writes ListModelsForTests; only a test may answer for a harness", at)
	}
}

// ProcessesForTests is the whole process table the shared-home check reads.
// A shipped yad that set it would judge a home shared or not from a table it
// made up, and TestMain sets it for every test here, so nothing else would
// notice.
func TestOnlyTestsReachTheProcessTable(t *testing.T) {
	for _, at := range seamtest.Writes(t, "../..", "ProcessesForTests") {
		t.Errorf("%s writes ProcessesForTests; only a test may stand in for ps", at)
	}
}
