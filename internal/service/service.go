// Package service is the Windows service plumbing, isolated so that main can
// read the same on every platform.
//
// The binary is both the service and its own installer. One file to copy to a
// counter, one command to register it, and no separate installer to keep in
// step with it.
package service

import "context"

// Work is what the service supervises. Start must return once the thing is
// listening; Stop must return once it has let go of its socket.
type Work struct {
	Start func() error
	Stop  func(context.Context) error
}

// Name is the service key, display name and description.
const (
	Name        = "Thr1vePrint"
	DisplayName = "Thr1ve Print Agent"
	Description = "Prints receipts and opens the cash drawer for the Thr1ve front desk."
)
