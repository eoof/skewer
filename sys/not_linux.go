// +build !linux

package sys

type NotLinuxError struct{}

var CapabilitiesSupported bool = false

func (e NotLinuxError) Error() string {
	return "Only available on Linux"
}

func SetNonDumpable() error {
	return NotLinuxError{}
}

func FixLinuxPrivileges(uid int, gid int) error {
	return nil
}

func CanReadAuditLogs() bool {
	return false
}