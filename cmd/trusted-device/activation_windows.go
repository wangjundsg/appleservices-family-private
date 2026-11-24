//go:build windows

package main

import (
	"errors"
	"os/exec"
	"os/user"
	"syscall"
)

func hideActivationProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
func protectActivationPath(path string, directory bool) error {
	owner, err := user.Current()
	if err != nil {
		return err
	}
	rights := "F"
	if directory {
		rights = "(OI)(CI)F"
	}
	command := exec.Command("icacls.exe", path, "/inheritance:r", "/grant:r", "*"+owner.Uid+":"+rights)
	hideActivationProcess(command)
	if command.Run() != nil {
		return errors.New("activation_private_permissions_failed")
	}
	return nil
}
