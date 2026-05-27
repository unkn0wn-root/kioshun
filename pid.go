package cache

import _ "unsafe" // for go:linkname

// runtime_procPin/runtime_procUnpin expose the current P only as a fast stripe
// hint for lossy read sampling; no correctness path depends on the value.
//
//go:linkname runtime_procPin runtime.procPin
func runtime_procPin() int

//go:linkname runtime_procUnpin runtime.procUnpin
func runtime_procUnpin()

// procID returns a "best effort" P index for spreading lock-free producers.
func procID() int {
	pid := runtime_procPin()
	runtime_procUnpin()
	return pid
}
