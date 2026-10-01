//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package cas

// fsIocGetversion is FS_IOC_GETVERSION, which reads an inode's
// generation: ext4, XFS and btrfs change it when they reuse the inode.
// Its number declares a long, 8 bytes here; the kernel writes a 4-byte
// int.
const fsIocGetversion = 0x80087601
