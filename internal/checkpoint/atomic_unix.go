//go:build !windows

package checkpoint

func syncDirectory(path string) error {
	dir, err := openCheckpointPathRead(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
