//go:build !darwin && !linux

package main

func warnIfServiceManaged() bool {
	return false
}
