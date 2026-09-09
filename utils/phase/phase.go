// Package phase provides env-gated phase timers for rip profiling.
//
// When AMDL_PHASES=1, Since() prints "<label> <elapsed-ms>" lines to stderr
// so the seedbox test harness can attribute a rip's wall time to its phases
// (catalog lookups / playlist prep / download / decrypt+mux). When unset
// (production), every call is a no-op and the binary behaves identically.
package phase

import (
	"fmt"
	"os"
	"time"
)

var on = os.Getenv("AMDL_PHASES") == "1"

// Start returns the base timestamp for subsequent Since() deltas.
func Start() time.Time { return time.Now() }

// Since logs the wall time since t under the given label (stderr, gated).
func Since(t time.Time, label string) {
	if !on {
		return
	}
	fmt.Fprintf(os.Stderr, "[ph] %s %dms\n", label, time.Since(t).Milliseconds())
}
