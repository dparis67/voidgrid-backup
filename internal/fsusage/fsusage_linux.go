package fsusage

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/voidgrid/voidgrid-backup/internal/safeint"
)

func statfs(p string) (total, used, avail int64, err error) {
	var st unix.Statfs_t
	if err = unix.Statfs(p, &st); err != nil {
		return 0, 0, 0, err
	}
	// Some pseudo filesystems report all ones for a count they do not track, so
	// these saturate instead of wrapping to a negative size.
	bs := int64(st.Bsize)
	total = safeint.MulBytes(st.Blocks, bs)
	avail = safeint.MulBytes(st.Bavail, bs)
	used = max(total-safeint.MulBytes(st.Bfree, bs), 0)
	return total, used, avail, nil
}

func deviceOf(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
