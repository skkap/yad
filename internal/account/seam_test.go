package account

import (
	"testing"

	"github.com/skkap/yad/internal/seamtest"
)

// SecurityForTests names the program every Keychain delete runs, and makes
// any machine one that keeps Claude's logins in a Keychain. A shipped yad
// that set it would hand the Keychain's item names to whatever it named, on
// every removal and every token stored, so only a test may write it. The
// check is the source, parsed: any shipped file of the module that writes it,
// the file that declares it included — seamtest.Writes says what counts as a
// write, and its own tests prove it sees each one.
func TestOnlyTestsReachTheKeychainSeam(t *testing.T) {
	if SecurityForTests != "" {
		t.Fatalf("a test left the Keychain seam set to %q", SecurityForTests)
	}
	for _, at := range seamtest.Writes(t, "../..", "SecurityForTests") {
		t.Errorf("%s writes SecurityForTests; only a test may point the Keychain at another program", at)
	}
}
