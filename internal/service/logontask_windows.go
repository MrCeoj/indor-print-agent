//go:build windows

package service

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"unicode/utf16"
)

// TaskName is the scheduled task LogonTask mode registers.
const TaskName = "Thr1vePrintAgent"

// RegisterLogonTask makes the agent start in each user's session at logon,
// replacing any previous registration. It is the MSI's custom action for
// MODE=LogonTask, which runs as LocalSystem.
//
// The task is written as XML because that is the only schtasks form that takes
// a group principal. The group is the Users SID, not its name: every account
// name in Windows is localised, and a Spanish install calls it Usuarios.
func RegisterLogonTask() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", exe, err)
	}

	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(exe)); err != nil {
		return err
	}

	// ExecutionTimeLimit PT0S is "never stop it". Task Scheduler's default is
	// three days, after which it quietly kills the agent mid-shift.
	task := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>%s</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Users">
      <GroupId>S-1-5-32-545</GroupId>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <Enabled>true</Enabled>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
  </Settings>
  <Actions Context="Users">
    <Exec>
      <Command>%s</Command>
      <Arguments>run</Arguments>
    </Exec>
  </Actions>
</Task>
`, Description, escaped.String())

	f, err := os.CreateTemp("", "thr1ve-task-*.xml")
	if err != nil {
		return fmt.Errorf("writing the task definition: %w", err)
	}
	defer os.Remove(f.Name())

	// schtasks reads the file as UTF-16, as the declaration above says.
	units := utf16.Encode([]rune(task))
	buf := make([]byte, 2+2*len(units))
	binary.LittleEndian.PutUint16(buf, 0xFEFF)
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[2+2*i:], u)
	}
	_, werr := f.Write(buf)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return fmt.Errorf("writing the task definition: %w", err)
	}

	out, err := exec.Command("schtasks", "/Create", "/TN", TaskName, "/XML", f.Name(), "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("registering the logon task: %w\n%s", err, out)
	}

	// Best effort: start it now for whoever is already logged in, rather than
	// at their next logon.
	_ = exec.Command("schtasks", "/Run", "/TN", TaskName).Run()
	return nil
}
