//go:build windows

package checkpoint

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var getProcessHandleCount = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")

func checkpointProcessResourceCount() (int, error) {
	var count uint32
	ok, _, callErr := getProcessHandleCount.Call(
		uintptr(windows.CurrentProcess()),
		uintptr(unsafe.Pointer(&count)),
	)
	if ok == 0 {
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			return 0, windows.ERROR_GEN_FAILURE
		}
		return 0, callErr
	}
	return int(count), nil
}
