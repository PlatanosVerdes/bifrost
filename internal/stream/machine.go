package stream

import "log"

// Machine holds the limits that depend on the hardware bifrost runs on, all set by MACHINE_SIZE so a
// bigger box raises them together.
type Machine struct {
	Surveys int // channels whose sources are checked at once
}

var machineSizes = map[string]Machine{
	"small":  {Surveys: 1}, // a Raspberry Pi
	"medium": {Surveys: 2},
	"large":  {Surveys: 4},
}

// MachineSize reads MACHINE_SIZE; anything else counts as small.
func MachineSize(size string) Machine {
	if m, ok := machineSizes[size]; ok {
		return m
	}
	log.Printf("MACHINE_SIZE=%q is not small, medium or large, using small", size)
	return machineSizes["small"]
}
