//go:build windows

package fixture

// WindowsSpecificFeature is only available on Windows.
func WindowsSpecificFeature() string {
	return "windows"
}
