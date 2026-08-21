package recordcleaner

import "math"

// freeBytes is not implemented on Windows (this deployment only ever runs
// on Linux - see build.sh cross-compiling GOOS=linux). Return an
// effectively-unlimited value so the min-free-space check never triggers
// on a Windows dev machine.
func freeBytes(_ string) (uint64, error) {
	return math.MaxUint64, nil
}
