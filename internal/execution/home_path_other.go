//go:build !windows

package execution

func canonicalAmbientHomeDirectory(path string) (string, error) {
	return canonicalHomeDirectory(path, "ambient home")
}
