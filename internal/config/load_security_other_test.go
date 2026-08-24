//go:build !unix && !windows

package config

import "errors"

func makeLegacyBroadConfigForTest(string) error {
	return errors.New("legacy permission fixtures are unavailable")
}
