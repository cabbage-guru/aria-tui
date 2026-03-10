//go:build !darwin

package hotkey

import "fmt"

func IsInstalled() bool {
	return false
}

func Install() error {
	return fmt.Errorf("global hotkey installation is only supported on macOS")
}

func Uninstall() error {
	return fmt.Errorf("global hotkey installation is only supported on macOS")
}

func Instructions() string {
	return "Global hotkey setup is only supported on macOS.\nOn Linux, bind 'aria-tui clip' to a hotkey in your desktop environment settings."
}
