//go:build !darwin && !windows

package main

func loginItemEnabled() bool     { return false }
func enableLoginItem() error     { return nil }
func disableLoginItem() error    { return nil }
