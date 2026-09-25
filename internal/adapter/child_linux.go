package adapter

import (
	"os/exec"
	"syscall"
)

func configureChildCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
