package main

import "log"

// machine holds the limits that depend on the hardware bifrost runs on, all set by MACHINE_SIZE so a
// bigger box raises them together.
type machine struct {
	surveys int // channels whose sources are checked at once
}

var machineSizes = map[string]machine{
	"small":  {surveys: 1}, // a Raspberry Pi
	"medium": {surveys: 2},
	"large":  {surveys: 4},
}

func machineSize(size string) machine {
	if m, ok := machineSizes[size]; ok {
		return m
	}
	log.Printf("MACHINE_SIZE=%q is not small, medium or large, using small", size)
	return machineSizes["small"]
}
