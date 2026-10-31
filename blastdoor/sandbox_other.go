//go:build !(linux && (amd64 || arm64))

package blastdoor

const sandboxSupported = false

// HardenProcess has nothing to do without the sandbox.
func HardenProcess() error { return nil }
