package fusefrontend

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/rfjakob/gocryptfs/internal/configfile"
	"github.com/rfjakob/gocryptfs/internal/cryptocore"
	"github.com/rfjakob/gocryptfs/internal/nametransform"
	"github.com/rfjakob/gocryptfs/internal/syscallcompat"
	"github.com/rfjakob/gocryptfs/internal/tlog"
)

// mkdirWithIv - create a new directory and corresponding diriv file. dirfd
// should be a handle to the parent directory, cName is the name of the new
// directory and mode specifies the access permissions to use.
func (n *Node) mkdirWithIv(dirfd int, cName string, mode uint32, caller *fuse.Caller) error {
	rn := n.rootNode()
	// Between the creation of the directory and the creation of gocryptfs.diriv
	// the directory is inconsistent. Take the lock to prevent other readers
	// from seeing it.
	rn.dirIVLock.Lock()
	defer rn.dirIVLock.Unlock()
	err := syscallcompat.MkdiratUser(dirfd, cName, mode, caller)
	if err != nil {
		return err
	}
	dirfd2, err := syscallcompat.Openat(dirfd, cName, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscallcompat.O_PATH, 0)
	if err == nil {
		// Create gocryptfs.diriv
		err = nametransform.WriteDirIVAt(dirfd2)
		syscall.Close(dirfd2)
	}
	if err != nil {
		// Delete inconsistent directory (missing gocryptfs.diriv!)
		err2 := syscallcompat.Unlinkat(dirfd, cName, unix.AT_REMOVEDIR)
		if err2 != nil {
			tlog.Warn.Printf("mkdirWithIv: rollback failed: %v", err2)
		}
	}
	return err
}

// Mkdir - FUSE call. Create a directory at "newPath" with permissions "mode".
//
// Symlink-safe through use of Mkdirat().
func (n *Node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rn := n.rootNode()
	newPath := filepath.Join(n.path(), name)
	if rn.isFiltered(newPath) {
		return nil, syscall.EPERM
	}
	dirfd, cName, err := rn.openBackingDir(newPath)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	defer syscall.Close(dirfd)
	var caller *fuse.Caller
	if rn.args.PreserveOwner {
		caller, _ = fuse.FromContext(ctx)
	}
	if rn.args.PlaintextNames {
		err = syscallcompat.MkdiratUser(dirfd, cName, mode, caller)
		return nil, fs.ToErrno(err)
	}

	// We need write and execute permissions to create gocryptfs.diriv.
	// Also, we need read permissions to open the directory (to avoid
	// race-conditions between getting and setting the mode).
	origMode := mode
	mode = mode | 0700

	// Handle long file name
	if nametransform.IsLongContent(cName) {
		// Create ".name"
		err = rn.nameTransform.WriteLongNameAt(dirfd, cName, newPath)
		if err != nil {
			return nil, fs.ToErrno(err)
		}

		// Create directory
		err = rn.mkdirWithIv(dirfd, cName, mode, caller)
		if err != nil {
			nametransform.DeleteLongNameAt(dirfd, cName)
			return nil, fs.ToErrno(err)
		}
	} else {
		err = rn.mkdirWithIv(dirfd, cName, mode, caller)
		if err != nil {
			return nil, fs.ToErrno(err)
		}
	}

	fd, err := syscallcompat.Openat(dirfd, cName,
		syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		tlog.Warn.Printf("Mkdir %q: Openat failed: %v", cName, err)
		return nil, fs.ToErrno(err)
	}
	defer syscall.Close(fd)

	// Get unique inode number
	var st syscall.Stat_t
	err = syscall.Fstat(fd, &st)
	if err != nil {
		tlog.Warn.Printf("Mkdir %q: Fstat failed: %v", cName, err)
		return nil, fs.ToErrno(err)
	}
	// Create child node
	ch := n.newChild(ctx, &st, out)

	// Set mode
	if origMode != mode {
		// Preserve SGID bit if it was set due to inheritance.
		origMode = uint32(st.Mode&^0777) | origMode
		err = syscall.Fchmod(fd, origMode)
		if err != nil {
			tlog.Warn.Printf("Mkdir %q: Fchmod %#o -> %#o failed: %v", cName, mode, origMode, err)
		}
	}

	return ch, 0
}

// Readdir - FUSE call.
//
// This function is symlink-safe through use of openBackingDir() and
// ReadDirIVAt().
func (n *Node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	rn := n.rootNode()
	p := n.path()
	dirName := filepath.Base(p)
	parentDirFd, cDirName, err := rn.openBackingDir(p)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	defer syscall.Close(parentDirFd)

	// Read ciphertext directory
	var cipherEntries []fuse.DirEntry
	fd, err := syscallcompat.Openat(parentDirFd, cDirName, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	defer syscall.Close(fd)
	cipherEntries, err = syscallcompat.Getdents(fd)
	if err != nil {
		return nil, fs.ToErrno(err)
	}
	// Get DirIV (stays nil if PlaintextNames is used)
	var cachedIV []byte
	if !rn.args.PlaintextNames {
		// Read the DirIV from disk
		cachedIV, err = nametransform.ReadDirIVAt(fd)
		if err != nil {
			tlog.Warn.Printf("OpenDir %q: could not read %s: %v", cDirName, nametransform.DirIVFilename, err)
			return nil, syscall.EIO
		}
	}
	// Decrypted directory entries
	var plain []fuse.DirEntry
	// Filter and decrypt filenames
	for i := range cipherEntries {
		cName := cipherEntries[i].Name
		if dirName == "." && cName == configfile.ConfDefaultName {
			// silently ignore "gocryptfs.conf" in the top level dir
			continue
		}
		if rn.args.PlaintextNames {
			plain = append(plain, cipherEntries[i])
			continue
		}
		if cName == nametransform.DirIVFilename {
			// silently ignore "gocryptfs.diriv" everywhere if dirIV is enabled
			continue
		}
		// Handle long file name
		isLong := nametransform.LongNameNone
		if rn.args.LongNames {
			isLong = nametransform.NameType(cName)
		}
		if isLong == nametransform.LongNameContent {
			cNameLong, err := nametransform.ReadLongNameAt(fd, cName)
			if err != nil {
				tlog.Warn.Printf("OpenDir %q: invalid entry %q: Could not read .name: %v",
					cDirName, cName, err)
				rn.reportMitigatedCorruption(cName)
				continue
			}
			cName = cNameLong
		} else if isLong == nametransform.LongNameFilename {
			// ignore "gocryptfs.longname.*.name"
			continue
		}
		name, err := rn.nameTransform.DecryptName(cName, cachedIV)
		if err != nil {
			tlog.Warn.Printf("OpenDir %q: invalid entry %q: %v",
				cDirName, cName, err)
			rn.reportMitigatedCorruption(cName)
			continue
		}
		// Override the ciphertext name with the plaintext name but reuse the rest
		// of the structure
		cipherEntries[i].Name = name
		plain = append(plain, cipherEntries[i])
	}

	return fs.NewListDirStream(plain), 0
}

// Rmdir - FUSE call.
//
// Symlink-safe through Unlinkat() + AT_REMOVEDIR.
func (n *Node) Rmdir(ctx context.Context, name string) (code syscall.Errno) {
	rn := n.rootNode()
	p := filepath.Join(n.path(), name)
	parentDirFd, cName, err := rn.openBackingDir(p)
	if err != nil {
		return fs.ToErrno(err)
	}
	defer syscall.Close(parentDirFd)
	if rn.args.PlaintextNames {
		// Unlinkat with AT_REMOVEDIR is equivalent to Rmdir
		err = unix.Unlinkat(parentDirFd, cName, unix.AT_REMOVEDIR)
		return fs.ToErrno(err)
	}
	// Unless we are running as root, we need read, write and execute permissions
	// to handle gocryptfs.diriv.
	permWorkaround := false
	var origMode uint32
	if !rn.args.PreserveOwner {
		var st unix.Stat_t
		err = syscallcompat.Fstatat(parentDirFd, cName, &st, unix.AT_SYMLINK_NOFOLLOW)
		if err != nil {
			return fs.ToErrno(err)
		}
		if st.Mode&0700 != 0700 {
			tlog.Debug.Printf("Rmdir: permWorkaround")
			permWorkaround = true
			// This cast is needed on Darwin, where st.Mode is uint16.
			origMode = uint32(st.Mode)
			err = syscallcompat.FchmodatNofollow(parentDirFd, cName, origMode|0700)
			if err != nil {
				tlog.Debug.Printf("Rmdir: permWorkaround: chmod failed: %v", err)
				return fs.ToErrno(err)
			}
		}
	}
	dirfd, err := syscallcompat.Openat(parentDirFd, cName,
		syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		tlog.Debug.Printf("Rmdir: Open: %v", err)
		return fs.ToErrno(err)
	}
	defer syscall.Close(dirfd)
	// Undo the chmod if removing the directory failed. This must run before
	// closing dirfd, so defer it after (defer is LIFO).
	if permWorkaround {
		defer func() {
			if code != 0 {
				err = unix.Fchmod(dirfd, origMode)
				if err != nil {
					tlog.Warn.Printf("Rmdir: permWorkaround: rollback failed: %v", err)
				}
			}
		}()
	}
retry:
	// Check directory contents
	children, err := syscallcompat.Getdents(dirfd)
	if err == io.EOF {
		// The directory is empty
		tlog.Warn.Printf("Rmdir: %q: %s is missing", cName, nametransform.DirIVFilename)
		err = unix.Unlinkat(parentDirFd, cName, unix.AT_REMOVEDIR)
		return fs.ToErrno(err)
	}
	if err != nil {
		tlog.Warn.Printf("Rmdir: Readdirnames: %v", err)
		return fs.ToErrno(err)
	}
	// MacOS sprinkles .DS_Store files everywhere. This is hard to avoid for
	// users, so handle it transparently here.
	if runtime.GOOS == "darwin" && len(children) <= 2 && haveDsstore(children) {
		err = unix.Unlinkat(dirfd, dsStoreName, 0)
		if err != nil {
			tlog.Warn.Printf("Rmdir: failed to delete blocking file %q: %v", dsStoreName, err)
			return fs.ToErrno(err)
		}
		tlog.Warn.Printf("Rmdir: had to delete blocking file %q", dsStoreName)
		goto retry
	}
	// If the directory is not empty besides gocryptfs.diriv, do not even
	// attempt the dance around gocryptfs.diriv.
	if len(children) > 1 {
		return fs.ToErrno(syscall.ENOTEMPTY)
	}
	// Move "gocryptfs.diriv" to the parent dir as "gocryptfs.diriv.rmdir.XYZ"
	tmpName := fmt.Sprintf("%s.rmdir.%d", nametransform.DirIVFilename, cryptocore.RandUint64())
	tlog.Debug.Printf("Rmdir: Renaming %s to %s", nametransform.DirIVFilename, tmpName)
	// The directory is in an inconsistent state between rename and rmdir.
	// Protect against concurrent readers.
	rn.dirIVLock.Lock()
	defer rn.dirIVLock.Unlock()
	err = syscallcompat.Renameat(dirfd, nametransform.DirIVFilename,
		parentDirFd, tmpName)
	if err != nil {
		tlog.Warn.Printf("Rmdir: Renaming %s to %s failed: %v",
			nametransform.DirIVFilename, tmpName, err)
		return fs.ToErrno(err)
	}
	// Actual Rmdir
	err = syscallcompat.Unlinkat(parentDirFd, cName, unix.AT_REMOVEDIR)
	if err != nil {
		// This can happen if another file in the directory was created in the
		// meantime, undo the rename
		err2 := syscallcompat.Renameat(parentDirFd, tmpName,
			dirfd, nametransform.DirIVFilename)
		if err2 != nil {
			tlog.Warn.Printf("Rmdir: Rename rollback failed: %v", err2)
		}
		return fs.ToErrno(err)
	}
	// Delete "gocryptfs.diriv.rmdir.XYZ"
	err = syscallcompat.Unlinkat(parentDirFd, tmpName, 0)
	if err != nil {
		tlog.Warn.Printf("Rmdir: Could not clean up %s: %v", tmpName, err)
	}
	// Delete .name file
	if nametransform.IsLongContent(cName) {
		nametransform.DeleteLongNameAt(parentDirFd, cName)
	}
	return 0
}

// Opendir is a FUSE call to check if the directory can be opened.
func (n *Node) Opendir(ctx context.Context) (errno syscall.Errno) {
	dirfd, cName, errno := n.prepareAtSyscall("")
	if errno != 0 {
		return
	}
	defer syscall.Close(dirfd)

	// Open backing directory
	fd, err := syscallcompat.Openat(dirfd, cName, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fs.ToErrno(err)
	}
	syscall.Close(fd)
	return 0
}