//go:build !windows

package service

import "errors"

// The service commands exist on every platform so that `main` has one shape,
// but only Windows has a service control manager. On a developer's Mac the
// agent is run in the foreground and that is the whole story.

var errNotWindows = errors.New("service management is Windows-only; run the agent in the foreground with `agent run`")

func IsService() bool { return false }

func Run(Work) error { return errNotWindows }

func Install() error   { return errNotWindows }
func Uninstall() error { return errNotWindows }
func Start() error     { return errNotWindows }
func Stop() error      { return errNotWindows }

func RegisterLogonTask() error { return errNotWindows }
