//go:build darwin

package fileprivacy

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const darwinACLExtended = uintptr(0x100)

var (
	libcACLDeleteFDNPAddr      uintptr
	libcACLFreeAddr            uintptr
	libcACLGetFDNPAddr         uintptr
	libcACLGetEntryAddr        uintptr
	libcFilesecInitAddr        uintptr
	libcFilesecSetPropertyAddr uintptr
	libcFilesecFreeAddr        uintptr
	libcFchmodxNPAddr          uintptr
)

func darwinLibcCall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, errno syscall.Errno)

//go:linkname darwinLibcCall syscall.syscall

//go:cgo_import_dynamic libc_acl_delete_fd_np acl_delete_fd_np "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_acl_free acl_free "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_acl_get_fd_np acl_get_fd_np "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_acl_get_entry acl_get_entry "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_filesec_init filesec_init "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_filesec_set_property filesec_set_property "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_filesec_free filesec_free "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_fchmodx_np fchmodx_np "/usr/lib/libSystem.B.dylib"

func removeExtendedACL(f *os.File) error {
	r1, _, errno := darwinLibcCall(libcACLDeleteFDNPAddr, f.Fd(), darwinACLExtended, 0)
	if int32(r1) != -1 || errors.Is(errno, syscall.ENOENT) {
		return nil
	}
	// APFS currently returns ENOTSUP from acl_delete_fd_np for an extended
	// ACL even though it supports descriptor-bound removal. This filesec path
	// is the same mechanism used by Apple's chmod -N implementation.
	if !errors.Is(errno, syscall.ENOTSUP) && !errors.Is(errno, syscall.EINVAL) {
		return fmt.Errorf("removing extended ACL: %w", errno)
	}
	return removeExtendedACLWithFilesec(f)
}

func removeExtendedACLWithFilesec(f *os.File) error {
	filesec, _, errno := darwinLibcCall(libcFilesecInitAddr, 0, 0, 0)
	if filesec == 0 {
		if errno == 0 {
			errno = syscall.EINVAL
		}
		return fmt.Errorf("initializing file security descriptor: %w", errno)
	}
	defer func() {
		_, _, _ = darwinLibcCall(libcFilesecFreeAddr, filesec, 0, 0)
	}()
	// FILESEC_ACL is property 5. Apple defines _FILESEC_REMOVE_ACL as
	// (void *)1. fchmodx_np applies it to the already-open descriptor.
	r1, _, errno := darwinLibcCall(libcFilesecSetPropertyAddr, filesec, 5, 1)
	if int32(r1) == -1 {
		return fmt.Errorf("marking extended ACL for removal: %w", errno)
	}
	r1, _, errno = darwinLibcCall(libcFchmodxNPAddr, f.Fd(), filesec, 0)
	if int32(r1) == -1 {
		return fmt.Errorf("removing extended ACL: %w", errno)
	}
	return nil
}

func hasExtendedACL(f *os.File) (bool, error) {
	acl, _, errno := darwinLibcCall(libcACLGetFDNPAddr, f.Fd(), darwinACLExtended, 0)
	if acl == 0 {
		// Darwin reports EINVAL when the object has no extended ACL to
		// materialize; some filesystems leave errno unchanged. ENOTSUP means
		// the filesystem has no ACL facility.
		if errno == 0 || errors.Is(errno, syscall.EINVAL) || errors.Is(errno, syscall.ENOTSUP) {
			return false, nil
		}
		return false, fmt.Errorf("reading extended ACL: %w", errno)
	}
	defer freeDarwinACL(acl)
	var entry uintptr
	r1, _, errno := darwinLibcCall(libcACLGetEntryAddr, acl, 0, uintptr(unsafe.Pointer(&entry)))
	if int32(r1) == -1 {
		if errors.Is(errno, syscall.EINVAL) {
			return false, nil
		}
		return false, fmt.Errorf("reading extended ACL entry: %w", errno)
	}
	return true, nil
}

func freeDarwinACL(acl uintptr) {
	_, _, _ = darwinLibcCall(libcACLFreeAddr, acl, 0, 0)
}
