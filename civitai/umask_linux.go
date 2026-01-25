package civitai

import "syscall"

func maybeUmask(mask int) (oldmask int) {
	return syscall.Umask(mask)
}
