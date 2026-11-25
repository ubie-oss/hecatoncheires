//go:build !darwin && !linux && !dragonfly && !freebsd && !netbsd && !openbsd

package harness

import (
	"os"

	"github.com/m-mizutani/goerr/v2"
)

const privateOpenFlags = 0

func ownedByThisUser(os.FileInfo) bool { return false }

func acquireSessionLock(*os.File) error {
	return goerr.New("harness: session directory locking is unavailable on this platform")
}
