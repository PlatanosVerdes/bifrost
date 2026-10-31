package stream

import "log"

// Machine holds the limits that depend on the hardware bifrost runs on, all set by MACHINE_SIZE so a
// bigger box raises them together.
type Machine struct {
	Surveys int // channels whose sources are checked at once
	// Bytes a viewer may fall behind before it is dropped; chunks are shared, so it bounds memory per
	// channel. 32 MB lasted 7 s on a 38 Mbit/s source.
	ViewerBacklog int
}

var machineSizes = map[string]Machine{
	"small":  {Surveys: 1, ViewerBacklog: 96 << 20}, // a Raspberry Pi
	"medium": {Surveys: 2, ViewerBacklog: 192 << 20},
	"large":  {Surveys: 4, ViewerBacklog: 384 << 20},
}

// MachineSize reads MACHINE_SIZE; anything else counts as small.
func MachineSize(size string) Machine {
	if m, ok := machineSizes[size]; ok {
		return m
	}
	log.Printf("MACHINE_SIZE=%q is not small, medium or large, using small", size)
	return machineSizes["small"]
}
