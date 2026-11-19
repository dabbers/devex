//go:build linux

package local

import "syscall"

func supportsRunAs() error { return nil }

// credential adds the run-as account to attr, creating attr when it is nil.
// Supplementary groups are set to none rather than inherited: the control
// plane's are root's, and keeping them would leave a workload root in all but
// name.
func (d *Driver) credential(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	if d.opts.RunAs == nil {
		return attr
	}
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.Credential = &syscall.Credential{
		Uid:    d.opts.RunAs.UID,
		Gid:    d.opts.RunAs.GID,
		Groups: []uint32{},
	}
	return attr
}
