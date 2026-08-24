//go:build darwin

#include "textflag.h"

TEXT libc_acl_delete_fd_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_acl_delete_fd_np(SB)
GLOBL	·libcACLDeleteFDNPAddr(SB), RODATA, $8
DATA	·libcACLDeleteFDNPAddr(SB)/8, $libc_acl_delete_fd_np_trampoline<>(SB)

TEXT libc_acl_free_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_acl_free(SB)
GLOBL	·libcACLFreeAddr(SB), RODATA, $8
DATA	·libcACLFreeAddr(SB)/8, $libc_acl_free_trampoline<>(SB)

TEXT libc_acl_get_fd_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_acl_get_fd_np(SB)
GLOBL	·libcACLGetFDNPAddr(SB), RODATA, $8
DATA	·libcACLGetFDNPAddr(SB)/8, $libc_acl_get_fd_np_trampoline<>(SB)

TEXT libc_acl_get_entry_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_acl_get_entry(SB)
GLOBL	·libcACLGetEntryAddr(SB), RODATA, $8
DATA	·libcACLGetEntryAddr(SB)/8, $libc_acl_get_entry_trampoline<>(SB)

TEXT libc_filesec_init_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_filesec_init(SB)
GLOBL	·libcFilesecInitAddr(SB), RODATA, $8
DATA	·libcFilesecInitAddr(SB)/8, $libc_filesec_init_trampoline<>(SB)

TEXT libc_filesec_set_property_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_filesec_set_property(SB)
GLOBL	·libcFilesecSetPropertyAddr(SB), RODATA, $8
DATA	·libcFilesecSetPropertyAddr(SB)/8, $libc_filesec_set_property_trampoline<>(SB)

TEXT libc_filesec_free_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_filesec_free(SB)
GLOBL	·libcFilesecFreeAddr(SB), RODATA, $8
DATA	·libcFilesecFreeAddr(SB)/8, $libc_filesec_free_trampoline<>(SB)

TEXT libc_fchmodx_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_fchmodx_np(SB)
GLOBL	·libcFchmodxNPAddr(SB), RODATA, $8
DATA	·libcFchmodxNPAddr(SB)/8, $libc_fchmodx_np_trampoline<>(SB)
