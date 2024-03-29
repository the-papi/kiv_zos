package vfsapi

import (
	"errors"
	"fmt"
	"github.com/PapiCZ/kiv_zos/vfs"
	"io"
	"strings"
)

type DirectoryIsNotEmpty struct {
	Name string
}

func (d DirectoryIsNotEmpty) Error() string {
	return fmt.Sprintf("directory %s is not empty", d.Name)
}

type File struct {
	filesystem   vfs.Filesystem
	mutableInode vfs.MutableInode
	offset       int
	name         string
}

func Exists(fs vfs.Filesystem, path string) (bool, error) {
	_, err := getInodeByPathRecursively(fs, path)
	if err != nil {
		switch err.(type) {
		case vfs.DirectoryEntryNotFound:
			return false, nil
		default:
			return false, err
		}
	}

	return true, nil
}

func Open(fs vfs.Filesystem, path string, create bool) (*File, error) {
	pathFragments := splitString(path, "/")
	parentPath := pathFragments[:len(pathFragments)-1]
	name := pathFragments[len(pathFragments)-1]

	mutableInode, err := getInodeByPathRecursively(fs, path)
	if err != nil {
		switch err.(type) {
		case vfs.DirectoryEntryNotFound:
			if !create {
				return nil, err
			}

			// Path doesn't exist, we want to create directory entry in parent inode
			parentMutableInode, err := getInodeByPathRecursively(fs, joinString(parentPath, "/"))
			if err != nil {
				return nil, err
			}

			// Create new file
			vo, err := vfs.FindFreeInode(fs.Volume, fs.Superblock, true)
			if err != nil {
				return nil, err
			}
			err = vfs.AppendDirectoryEntries(
				fs.Volume,
				fs.Superblock,
				parentMutableInode,
				vfs.NewDirectoryEntry(
					name,
					vfs.VolumePtrToInodePtr(fs.Superblock, vo.VolumePtr)),
			)
			if err != nil {
				return nil, err
			}

			mutableInode, err = getInodeByPathRecursively(fs, path)
			if err != nil {
				return nil, err
			}

		default:
			return nil, err
		}
	}

	return &File{
		filesystem:   fs,
		mutableInode: mutableInode,
		offset:       0,
		name:         name,
	}, nil
}

func Mkdir(fs vfs.Filesystem, path string) error {
	pathFragments := splitString(path, "/")
	parentPath := pathFragments[:len(pathFragments)-1]
	name := pathFragments[len(pathFragments)-1]

	parentMutableInode, err := getInodeByPathRecursively(fs, joinString(parentPath, "/"))
	if err != nil {
		return err
	}

	// Check for duplicate entry
	_, _, err = vfs.FindDirectoryEntryByName(fs.Volume, fs.Superblock, *parentMutableInode.Inode, name)
	if err == nil {
		// We are trying to create duplicate entryw
		return vfs.DuplicateDirectoryEntry{}
	}

	newDirInodeObj, err := vfs.FindFreeInode(fs.Volume, fs.Superblock, true)
	if err != nil {
		return err
	}
	newDirInode := newDirInodeObj.Object.(vfs.Inode)
	newDirInode.Type = vfs.InodeDirectoryType
	newDirInodeObj.Object = newDirInode
	err = newDirInodeObj.Save()
	if err != nil {
		return err
	}

	// Create new directory in parent newDirInode
	err = vfs.AppendDirectoryEntries(
		fs.Volume,
		fs.Superblock,
		parentMutableInode,
		vfs.NewDirectoryEntry(
			name,
			vfs.VolumePtrToInodePtr(fs.Superblock, newDirInodeObj.VolumePtr)),
	)
	if err != nil {
		return err
	}

	// Initialize newly created directory (add . and ..)
	err = vfs.AppendDirectoryEntries(
		fs.Volume,
		fs.Superblock,
		vfs.MutableInode{
			Inode:    &newDirInode,
			InodePtr: vfs.VolumePtrToInodePtr(fs.Superblock, newDirInodeObj.VolumePtr),
		},
		vfs.NewDirectoryEntry(
			".",
			vfs.VolumePtrToInodePtr(fs.Superblock, newDirInodeObj.VolumePtr)),
		vfs.NewDirectoryEntry(
			"..",
			parentMutableInode.InodePtr),
	)
	if err != nil {
		return err
	}

	return nil
}

func Remove(fs vfs.Filesystem, path string) error {
	cwd, err := Abs(fs, ".")
	if err != nil {
		return err
	}

	abs, err := Abs(fs, path)
	if err != nil {
		return err
	}

	if abs == cwd {
		return errors.New("can't delete current working directory")
	}

	pathFragments := splitString(path, "/")
	parentPath := pathFragments[:len(pathFragments)-1]
	name := pathFragments[len(pathFragments)-1]

	if name == "." || name == ".." {
		return errors.New("you can't delete \".\" or \"..\"")
	}

	parentMutableInode, err := getInodeByPathRecursively(fs, joinString(parentPath, "/"))
	if err != nil {
		return err
	}

	fileMutableInode, err := getInodeByPathRecursively(fs, path)
	if err != nil {
		return err
	}

	if fileMutableInode.Inode.IsRootDir() {
		return errors.New("cannot remove root directory")
	}

	// Check if file is dir and empty
	if fileMutableInode.Inode.IsDir() {
		directoryEntries, err := vfs.ReadAllDirectoryEntries(fs.Volume, fs.Superblock, *fileMutableInode.Inode)
		if err != nil {
			return err
		}

		// Empty directory contains 2 directory entries (. and ..)
		if len(directoryEntries) != 2 {
			return DirectoryIsNotEmpty{Name: path}
		}
	}

	// Free clusters
	_, err = vfs.Shrink(fileMutableInode, fs.Volume, fs.Superblock, 0)
	if err != nil {
		return err
	}

	// Free inode
	err = vfs.FreeInode(fs.Volume, fs.Superblock, fileMutableInode.InodePtr)
	if err != nil {
		return err
	}

	// Remove directory entry
	_, err = vfs.RemoveDirectoryEntry(fs.Volume, fs.Superblock, parentMutableInode, name)
	if err != nil {
		return err
	}

	return nil
}

func BadRemove(fs vfs.Filesystem, path string) error {
	pathFragments := splitString(path, "/")
	parentPath := pathFragments[:len(pathFragments)-1]
	name := pathFragments[len(pathFragments)-1]

	parentMutableInode, err := getInodeByPathRecursively(fs, joinString(parentPath, "/"))
	if err != nil {
		return err
	}

	fileMutableInode, err := getInodeByPathRecursively(fs, path)
	if err != nil {
		return err
	}

	if fileMutableInode.Inode.IsRootDir() {
		return errors.New("cannot remove root directory")
	}

	// Check if file is dir and empty
	if fileMutableInode.Inode.IsDir() {
		directoryEntries, err := vfs.ReadAllDirectoryEntries(fs.Volume, fs.Superblock, *fileMutableInode.Inode)
		if err != nil {
			return err
		}

		// Empty directory contains 2 directory entries (. and ..)
		if len(directoryEntries) != 2 {
			return DirectoryIsNotEmpty{Name: path}
		}
	}

	// Free clusters
	_, err = vfs.Shrink(fileMutableInode, fs.Volume, fs.Superblock, 0)
	if err != nil {
		return err
	}

	// Remove directory entry
	_, err = vfs.RemoveDirectoryEntry(fs.Volume, fs.Superblock, parentMutableInode, name)
	if err != nil {
		return err
	}

	return nil
}


func Rename(fs vfs.Filesystem, oldPath, newPath string) error {
	// Build variables for new path
	newPathFragments := splitString(newPath, "/")
	newParentPath := newPathFragments[:len(newPathFragments)-1]
	newName := newPathFragments[len(newPathFragments)-1]

	// Find new parent inode
	newParentMutableInode, err := getInodeByPathRecursively(fs, joinString(newParentPath, "/"))
	if err != nil {
		return err
	}

	// Check for duplicate entry
	_, _, err = vfs.FindDirectoryEntryByName(fs.Volume, fs.Superblock, *newParentMutableInode.Inode, newName)
	if err == nil {
		// We are trying to create duplicate entry
		return vfs.DuplicateDirectoryEntry{}
	}

	// Build variables for old path
	oldPathFragments := splitString(oldPath, "/")
	oldParentPath := oldPathFragments[:len(oldPathFragments)-1]
	oldName := oldPathFragments[len(oldPathFragments)-1]

	if oldName == "." || oldName == ".."  {
		return errors.New("you can't rename \".\" or \"..\"")
	}

	// Find old parent inode
	oldParentMutableInode, err := getInodeByPathRecursively(fs, joinString(oldParentPath, "/"))
	if err != nil {
		return err
	}

	// Remove directory entry from parent inode
	directoryEntry, err := vfs.RemoveDirectoryEntry(fs.Volume, fs.Superblock, oldParentMutableInode, oldName)
	if err != nil {
		return err
	}

	err = newParentMutableInode.Reload(fs.Volume, fs.Superblock)
	if err != nil {
		return err
	}

	directoryEntry.Name = vfs.StringNameToBytes(newName)

	err = vfs.AppendDirectoryEntries(fs.Volume, fs.Superblock, newParentMutableInode, directoryEntry)
	if err != nil {
		return err
	}

	return nil
}

func ChangeDirectory(fs *vfs.Filesystem, path string) error {
	mutableInode, err := getInodeByPathRecursively(*fs, path)
	if err != nil {
		return err
	}

	fs.CurrentInodePtr = mutableInode.InodePtr

	return nil
}

func Abs(fs vfs.Filesystem, path string) (string, error) {
	pathFragments := splitString(path, "/")
	parentPath := pathFragments[:len(pathFragments)-1]
	name := pathFragments[len(pathFragments)-1]
	filename := ""

	file, err := Open(fs, path, false)
	if err != nil {
		return "", err
	}
	if !file.IsDir() {
		// File
		filename = name
		path = joinString(parentPath, "/")
	}

	mutableInode, err := getInodeByPathRecursively(fs, path)
	if err != nil {
		return "", err
	}

	pathFragments = make([]string, 0)
	for {
		parentMutableInode, err := getInodeByPathFromInodeRecursively(fs, mutableInode.InodePtr, "..")
		if err != nil {
			return "", err
		}

		_, directoryEntry, err := vfs.FindDirectoryEntryByInodePtr(fs.Volume, fs.Superblock, *parentMutableInode.Inode, mutableInode.InodePtr)
		if err != nil {
			return "", err
		}

		mutableInode = parentMutableInode
		pathFragments = append(pathFragments, cToGoString(directoryEntry.Name[:]))

		if mutableInode.InodePtr == fs.RootInodePtr {
			break
		}
	}

	// Reverse path fragments order
	for i := len(pathFragments)/2 - 1; i >= 0; i-- {
		opp := len(pathFragments) - 1 - i
		pathFragments[i], pathFragments[opp] = pathFragments[opp], pathFragments[i]
	}

	if len(filename) != 0 {
		pathFragments = append(pathFragments, filename)
	}

	return "/" + strings.Join(pathFragments, "/"), nil
}

func DataClustersInfo(fs vfs.Filesystem, path string) ([]vfs.ClusterPtr,
	map[vfs.ClusterPtr][]vfs.ClusterPtr,
	map[vfs.ClusterPtr]map[vfs.ClusterPtr][]vfs.ClusterPtr,
	error) {

	mutableInode, err := getInodeByPathRecursively(fs, path)
	if err != nil {
		return nil, nil, nil, err
	}

	return mutableInode.Inode.GetUsedPtrs(fs.Volume, fs.Superblock)
}

func (f File) ReadDir() ([]FileInfo, error) {
	if !f.IsDir() {
		return nil, errors.New("file is not a directory")
	}

	fileInfos := make([]FileInfo, 0)

	directoryEntries, err := vfs.ReadAllDirectoryEntries(f.filesystem.Volume, f.filesystem.Superblock, *f.mutableInode.Inode)
	if err != nil {
		return fileInfos, err
	}

	for _, directoryEntry := range directoryEntries {
		mutableInode, err := vfs.LoadMutableInode(f.filesystem.Volume, f.filesystem.Superblock, directoryEntry.InodePtr)
		if err != nil {
			return fileInfos, err
		}
		fileInfos = append(fileInfos, FileInfo{
			name:     cToGoString(directoryEntry.Name[:]),
			size:     int(mutableInode.Inode.Size),
			inodePtr: int(mutableInode.InodePtr),
			isDir:    mutableInode.Inode.IsDir(),
		})
	}

	return fileInfos, nil
}

func (f *File) Write(data []byte) (int, error) {
	if f.IsDir() {
		return 0, errors.New("you can't write to directory")
	}

	n, err := f.mutableInode.WriteData(
		f.filesystem.Volume,
		f.filesystem.Superblock,
		vfs.VolumePtr(f.offset),
		data,
	)
	if err != nil {
		return int(n), err
	}

	f.offset += int(n)

	return int(n), nil
}

func (f *File) Read(data []byte) (int, error) {
	if f.IsDir() {
		return 0, errors.New("you can't read from directory")
	}

	if f.offset >= int(f.mutableInode.Inode.Size) {
		return 0, io.EOF
	}

	n, err := f.mutableInode.Inode.ReadData(
		f.filesystem.Volume,
		f.filesystem.Superblock,
		vfs.VolumePtr(f.offset),
		data,
	)
	if err != nil {
		return int(n), err
	}

	f.offset += int(n)

	if n == 0 && f.offset >= int(f.mutableInode.Inode.Size) {
		return int(n), io.EOF
	}

	return int(n), nil
}

func (f *File) ReadAll() (int, []byte, error) {
	data := make([]byte, f.mutableInode.Inode.Size)
	n, err := f.Read(data)

	return n, data, err
}

func (f File) IsDir() bool {
	return f.mutableInode.Inode.IsDir()
}

func (f File) Name() string {
	return f.name
}

func (f File) Size() int64 {
	return int64(f.mutableInode.Inode.Size)
}

func (f File) InodePtr() int64 {
	return int64(f.mutableInode.InodePtr)
}
