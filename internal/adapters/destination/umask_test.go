package destination

import "syscall"

// umaskZero clears the umask and returns the restore function.
func umaskZero() func() {
	old := syscall.Umask(0)
	return func() { syscall.Umask(old) }
}
