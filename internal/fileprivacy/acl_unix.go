//go:build unix && !darwin

package fileprivacy

import "os"

func removeExtendedACL(*os.File) error      { return nil }
func hasExtendedACL(*os.File) (bool, error) { return false, nil }
