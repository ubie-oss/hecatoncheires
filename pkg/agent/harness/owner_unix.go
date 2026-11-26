//go:build darwin || linux || dragonfly || freebsd || netbsd || openbsd

package harness

import (
	"os"
	"syscall"

	"github.com/m-mizutani/goerr/v2"
)

// privateOpenFlags refuses symlinks and avoids blocking on a planted FIFO.
const privateOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// ownedByThisUser reports whether info describes something this process's user
// owns. It fails closed: an entry whose owner cannot be read is not ours.
func ownedByThisUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	euid := os.Geteuid()
	return euid >= 0 && uint64(st.Uid) == uint64(euid) // #nosec G115 -- euid is checked non-negative
}

func acquireSessionLock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return goerr.Wrap(err, "harness: cannot acquire the session directory lock")
	}
	return nil
}
