package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/KishanBagaria/ipswrun/dyldcache"
)

// platformOf is the platform of the system an OS of osOf's runs.
func platformOf(system string) dyldcache.Platform {
	if system == iOS {
		return dyldcache.IOS
	}
	return dyldcache.MacOS
}

// checkDevice refuses an iPhone or iPad --device with a chip before the A12, before anything is
// looked up for it: its iOS has only an arm64 shared cache, and ipswrun runs the arm64e one.
func checkDevice(device string) error {
	product, _, _ := strings.Cut(device, ",")
	var name string
	var oldest int
	switch {
	case strings.HasPrefix(product, "iPhone"):
		name, oldest = "iPhone", 11
	case strings.HasPrefix(product, "iPad"):
		name, oldest = "iPad", 8
	default:
		return nil
	}
	if major, err := strconv.Atoi(strings.TrimPrefix(product, name)); err == nil && major < oldest {
		return fmt.Errorf("--device %s has a chip before the A12, whose iOS has no arm64e shared cache; pass an iPhone from iPhone11,2 on, or an iPad from iPad8,1 on", device)
	}
	return nil
}
