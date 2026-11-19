package proctrack

// killIdentity cannot signal one process safely on macOS: there is no handle
// that pins a PID, so a reused PID could receive the signal.
func killIdentity(ident) error { return errNoPidfd }
