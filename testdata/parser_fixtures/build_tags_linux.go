//go:build linux

package fixture

// LinuxSpecificFeature is only available on Linux.
func LinuxSpecificFeature() string {
	return "linux"
}
