//go:build !darwin

package runner

import "testing"

// canFreeze is false because the only immutable flag Linux has (chattr +i)
// takes CAP_LINUX_IMMUTABLE, so the case nothing gets past is proven on
// darwin alone. A test checks it before calling freeze.
const canFreeze = false

func freeze(t *testing.T, _ string) {
	t.Error("freeze called where canFreeze is false")
}
