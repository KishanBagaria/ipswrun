package main

import "testing"

func TestCheckDevice(t *testing.T) {
	for device, ok := range map[string]bool{
		"iPhone11,2": true, "iPhone17,1": true, "iPad8,1": true, "iPad13,4": true, "Macmini9,1": true,
		"iPhone10,3": false, "iPad7,5": false,
	} {
		if err := checkDevice(device); (err == nil) != ok {
			t.Errorf("checkDevice(%s) = %v", device, err)
		}
	}
}
