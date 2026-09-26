//go:build unix

package devfile

import (
	"io/fs"
	"os"
	"syscall"
)

const supported = true

// openNoFollow refuses to open a final path component that is a symlink.
const openNoFollow = syscall.O_NOFOLLOW

func platformOwner(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func platformEUID() uint32 { return uint32(os.Geteuid()) }
