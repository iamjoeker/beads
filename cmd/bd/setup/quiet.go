package setup

import (
	"io"
	"os"
	"sync/atomic"
)

// quiet suppresses the installers' progress narration.
//
// Every Install*/Uninstall* entry point narrates what it is doing to stdout
// unconditionally. That is correct for `bd setup claude`, where the narration
// IS the command's output, and wrong for `bd init --quiet`, whose contract is
// "print nothing on success": init gated its own messages and the installers'
// FAILURES on quiet, but the installers' success narration went straight to
// os.Stdout, so a quiet init still emitted ~25 lines beginning "Installing
// Claude hooks for this project..." (bd-kbx).
//
// This is a package-level toggle rather than a parameter on the entry points
// because the writer is chosen inside each backend's default env constructor,
// several call layers below them, and one entry point is reached through a
// domain adapter interface (internal/storage/domain.BeadsDirFSUseCase) that
// non-CLI callers share.
var quiet atomic.Bool

// SetQuiet routes installer progress output to io.Discard while enabled, and
// returns a function that restores the previous setting. Errors are
// unaffected: quiet suppresses narration, not failures, which the installers
// report to stderr and through their returned error.
func SetQuiet(on bool) (restore func()) {
	prev := quiet.Swap(on)
	return func() { quiet.Store(prev) }
}

// progressWriter is where the installers narrate their progress.
func progressWriter() io.Writer {
	if quiet.Load() {
		return io.Discard
	}
	return os.Stdout
}
