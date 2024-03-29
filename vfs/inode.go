package vfs

import (
	"fmt"
	"math"
	"unsafe"
)

const InodeDirectCount = 5

type ClusterIndexOutOfRange struct {
	index ClusterPtr
}

func (c ClusterIndexOutOfRange) Error() string {
	return fmt.Sprintf("index out of range [%d], maximal index is [%d]", c.index, 0)
}

const (
	InodeFileType      = 0
	InodeDirectoryType = 1
	InodeRootInodeType = 2
)

type Inode struct {
	Type              byte
	Size              VolumePtr
	AllocatedClusters ClusterPtr
	Direct1           ClusterPtr
	Direct2           ClusterPtr
	Direct3           ClusterPtr
	Direct4           ClusterPtr
	Direct5           ClusterPtr
	Indirect1         ClusterPtr
	Indirect2         ClusterPtr
}

func NewInode() Inode {
	return Inode{
		Direct1:   Unused,
		Direct2:   Unused,
		Direct3:   Unused,
		Direct4:   Unused,
		Direct5:   Unused,
		Indirect1: Unused,
		Indirect2: Unused,
	}
}

func (i Inode) ReadData(volume ReadWriteVolume, sb Superblock, offset VolumePtr, data []byte) (VolumePtr, error) {
	clusterPtrOffset := ClusterPtr(offset / VolumePtr(sb.ClusterSize))
	offsetInCluster := offset % VolumePtr(sb.ClusterSize)

	dataOffset := VolumePtr(0)
	for {
		clusterPtr, err := i.ResolveDataClusterAddress(volume, sb, clusterPtrOffset)
		if err != nil {
			return dataOffset, err
		}

		var clusterDataLength VolumePtr
		if i.AllocatedClusters-1 == clusterPtrOffset {
			// Last cluster
			clusterDataLength = i.Size % VolumePtr(sb.ClusterSize)
			if clusterDataLength == 0 {
				clusterDataLength = VolumePtr(sb.ClusterSize)
			}
		} else {
			clusterDataLength = VolumePtr(sb.ClusterSize)
		}

		// Apply offset
		clusterDataLength -= offsetInCluster

		if clusterDataLength > VolumePtr(len(data))-dataOffset {
			clusterDataLength = VolumePtr(len(data)) - dataOffset
		}

		clusterData := make([]byte, clusterDataLength)
		err = volume.ReadBytes(ClusterPtrToVolumePtr(sb, clusterPtr)+offsetInCluster, clusterData)
		if err != nil {
			return dataOffset, err
		}

		copy(data[dataOffset:], clusterData)
		clusterPtrOffset++
		offsetInCluster = 0
		dataOffset += VolumePtr(len(clusterData))

		if dataOffset >= VolumePtr(len(data)) || offset+dataOffset == i.Size {
			break
		}
	}

	return dataOffset, nil
}

func (i Inode) ResolveDataClusterAddress(volume ReadWriteVolume, sb Superblock, index ClusterPtr) (ClusterPtr, error) {
	// TODO: math.Ceil?
	if index >= i.AllocatedClusters {
		return 0, ClusterIndexOutOfRange{index}
	}

	// Resolve direct
	if index == 0 {
		return i.Direct1, nil
	} else if index == 1 {
		return i.Direct2, nil
	} else if index == 2 {
		return i.Direct3, nil
	} else if index == 3 {
		return i.Direct4, nil
	} else if index == 4 {
		return i.Direct5, nil
	}

	ptrsPerCluster := ClusterPtr(getPtrsPerCluster(sb))
	if index >= InodeDirectCount && index < InodeDirectCount+ptrsPerCluster {
		// Resolve indirect1
		indexInIndirect1 := index - InodeDirectCount

		data := make([]byte, sb.ClusterSize)
		err := volume.ReadBytes(ClusterPtrToVolumePtr(sb, i.Indirect1), data)
		if err != nil {
			return 0, err
		}
		dataClusterPtrs := GetClusterPtrsFromBinary(data)
		return dataClusterPtrs[indexInIndirect1], nil
	} else {
		// Resolve indirect2
		indexInIndirect2 := index - (InodeDirectCount + ptrsPerCluster)

		doublePtrData := make([]byte, sb.ClusterSize)
		err := volume.ReadBytes(ClusterPtrToVolumePtr(sb, i.Indirect2), doublePtrData)
		if err != nil {
			return 0, err
		}
		singleClusterPtrs := GetClusterPtrsFromBinary(doublePtrData)

		singlePtrDataIndex := indexInIndirect2 / ptrsPerCluster
		singlePtrData := make([]byte, sb.ClusterSize)
		err = volume.ReadBytes(ClusterPtrToVolumePtr(sb, singleClusterPtrs[singlePtrDataIndex]), singlePtrData)
		if err != nil {
			return 0, err
		}
		dataClusterPtrs := GetClusterPtrsFromBinary(singlePtrData)

		dataPtrIndex := indexInIndirect2 % ptrsPerCluster

		return dataClusterPtrs[dataPtrIndex], nil
	}
}

func (i Inode) GetUsedPtrs(volume ReadWriteVolume, sb Superblock) (
	[]ClusterPtr,
	map[ClusterPtr][]ClusterPtr,
	map[ClusterPtr]map[ClusterPtr][]ClusterPtr,
	error) {

	directPtrs := i.GetUsedDirectPtrs()
	indirect1Ptrs, err := i.GetUsedIndirect1Ptrs(volume, sb)
	if err != nil {
		return nil, nil, nil, err
	}

	indirect2Ptrs, err := i.GetUsedIndirect2Ptrs(volume, sb)
	if err != nil {
		return nil, nil, nil, err
	}

	return directPtrs, indirect1Ptrs, indirect2Ptrs, nil
}

func (i Inode) GetUsedDirectPtrs() []ClusterPtr {
	out := make([]ClusterPtr, 0)
	directPtrs := []ClusterPtr{
		i.Direct1,
		i.Direct2,
		i.Direct3,
		i.Direct4,
		i.Direct5,
	}

	for _, directPtr := range directPtrs {
		if directPtr != Unused {
			out = append(out, directPtr)
		}
	}

	return out
}

func (i Inode) GetUsedIndirect1Ptrs(volume ReadWriteVolume, sb Superblock) (map[ClusterPtr][]ClusterPtr, error) {
	out := make(map[ClusterPtr][]ClusterPtr)

	// Load pointers from data cluster
	ptrs := make([]ClusterPtr, getPtrsPerCluster(sb))
	err := volume.ReadStruct(ClusterPtrToVolumePtr(sb, i.Indirect1), ptrs)
	if err != nil {
		return nil, err
	}

	out[i.Indirect1] = ptrs[:allocatedDataClustersInIndirect1(i, sb)]

	return out, nil
}

func (i Inode) GetUsedIndirect2Ptrs(volume ReadWriteVolume, sb Superblock) (map[ClusterPtr]map[ClusterPtr][]ClusterPtr, error) {
	out := make(map[ClusterPtr]map[ClusterPtr][]ClusterPtr)
	remainingIndirect2DataPtrsCount := allocatedDataClustersInIndirect2(i, sb)
	doublePtrsCount := ClusterPtr(math.Ceil(float64(remainingIndirect2DataPtrsCount) / float64(getPtrsPerCluster(sb))))

	doublePtrs := make([]ClusterPtr, doublePtrsCount)
	err := volume.ReadStruct(ClusterPtrToVolumePtr(sb, i.Indirect2), doublePtrs)
	if err != nil {
		return nil, err
	}

	out[i.Indirect2] = make(map[ClusterPtr][]ClusterPtr)
	for _, doublePtr := range doublePtrs {
		singlePtrsCount := getPtrsPerCluster(sb)
		if singlePtrsCount > VolumePtr(remainingIndirect2DataPtrsCount) {
			singlePtrsCount = VolumePtr(remainingIndirect2DataPtrsCount)
		}

		singlePtrs := make([]ClusterPtr, singlePtrsCount)
		err := volume.ReadStruct(ClusterPtrToVolumePtr(sb, doublePtr), singlePtrs)
		if err != nil {
			return nil, err
		}

		out[i.Indirect2][doublePtr] = singlePtrs

		remainingIndirect2DataPtrsCount -= ClusterPtr(len(singlePtrs))
		if remainingIndirect2DataPtrsCount <= 0 {
			break
		}
	}

	return out, nil
}

func (i Inode) IsFile() bool {
	return i.Type == InodeFileType
}

func (i Inode) IsDir() bool {
	return i.Type == InodeDirectoryType || i.Type == InodeRootInodeType
}

func (i Inode) IsRootDir() bool {
	return i.Type == InodeRootInodeType
}

func GetClusterPtrsFromBinary(p []byte) []ClusterPtr {
	var cp ClusterPtr
	clusterPtrSize := int(unsafe.Sizeof(cp))
	ptrCount := len(p) / clusterPtrSize

	clusterPtrs := make([]ClusterPtr, 0, ptrCount)
	for i := 0; i < ptrCount; i++ {
		clusterBinaryPtr := p[i*clusterPtrSize : (i+1)*clusterPtrSize]

		var clusterPtr ClusterPtr
		for j := 0; j < clusterPtrSize; j++ {
			clusterPtr |= ClusterPtr(clusterBinaryPtr[j]) << ClusterPtr(8*j)
		}

		clusterPtrs = append(clusterPtrs, clusterPtr)
	}

	return clusterPtrs
}

type MutableInode struct {
	Inode    *Inode
	InodePtr InodePtr
}

func LoadMutableInode(volume ReadWriteVolume, sb Superblock, inodePtr InodePtr) (MutableInode, error) {
	inode := Inode{}
	err := volume.ReadStruct(InodePtrToVolumePtr(sb, inodePtr), &inode)
	if err != nil {
		return MutableInode{}, err
	}

	return MutableInode{
		Inode:    &inode,
		InodePtr: inodePtr,
	}, nil
}

func (mi MutableInode) AppendData(volume ReadWriteVolume, sb Superblock, data []byte) (n VolumePtr, err error) {
	return mi.WriteData(volume, sb, mi.Inode.Size, data)
}

func (mi MutableInode) WriteData(volume ReadWriteVolume, sb Superblock, offset VolumePtr, data []byte) (n VolumePtr, err error) {
	clusterIndex := ClusterPtr(offset / VolumePtr(sb.ClusterSize))
	indexInCluster := offset % VolumePtr(sb.ClusterSize)

	remainingDataLength := VolumePtr(len(data))
	writableSize := VolumePtr(math.Min(float64(remainingDataLength), float64(VolumePtr(sb.ClusterSize)-indexInCluster)))
	writtenData := VolumePtr(0)
	startIndex := VolumePtr(0)
	for {
		dataToWrite := make([]byte, writableSize)
		clusterPtr, err := mi.Inode.ResolveDataClusterAddress(volume, sb, clusterIndex)
		if err != nil {
			switch err.(type) {
			case ClusterIndexOutOfRange:
				// We need to allocate more space
				_, err = Allocate(mi, volume, sb, VolumePtr(sb.ClusterSize))
				if err != nil {
					return 0, err
				}
				clusterPtr, err = mi.Inode.ResolveDataClusterAddress(volume, sb, clusterIndex)
				if err != nil {
					return 0, err
				}
			default:
				return 0, err
			}
		}
		copy(dataToWrite, data[startIndex:startIndex+writableSize])
		err = volume.WriteStruct(ClusterPtrToVolumePtr(sb, clusterPtr)+indexInCluster, dataToWrite)
		if err != nil {
			return 0, err
		}

		indexInCluster = 0
		startIndex += writableSize
		writtenData += writableSize
		remainingDataLength -= writableSize
		if offset+writtenData > mi.Inode.Size {
			mi.Inode.Size = offset + writtenData
		}

		writableSize = VolumePtr(math.Min(float64(remainingDataLength), float64(VolumePtr(sb.ClusterSize))))
		clusterIndex++

		if remainingDataLength <= 0 {
			err = mi.Save(volume, sb)
			if err != nil {
				return writtenData, err
			}

			return writtenData, nil
		}
	}

}

func (mi MutableInode) Save(volume ReadWriteVolume, sb Superblock) error {
	return volume.WriteStruct(InodePtrToVolumePtr(sb, mi.InodePtr), mi.Inode)
}

func (mi MutableInode) Reload(volume ReadWriteVolume, sb Superblock) error {
	return volume.ReadStruct(InodePtrToVolumePtr(sb, mi.InodePtr), mi.Inode)
}
