//go:build linux && (386 || arm)

package cas

// fsIocGetversion is FS_IOC_GETVERSION, whose number declares a long, 4
// bytes here.
const fsIocGetversion = 0x80047601
