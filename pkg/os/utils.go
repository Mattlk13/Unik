package os

import (
	"errors"
	"os"
	"path"
	"text/template"

	log "github.com/Sirupsen/logrus"

	"github.com/emc-advanced-dev/unik/pkg/types"
)

const GrubTemplate = `default=0
fallback=1
timeout=1
hiddenmenu

title Unik
root {{.RootDrive}}
kernel /boot/program.bin {{.CommandLine}}
`

const DeviceMapFile = `(hd0) {{.GrubDevice}}
`

const ProgramName = "program.bin"

func createSparseFile(filename string, size DiskSize) error {
	fd, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer fd.Close()

	_, err = fd.Seek(int64(size.ToBytes())-1, 0)
	if err != nil {
		return err
	}
	_, err = fd.Write([]byte{0})
	if err != nil {
		return err
	}
	return nil
}

func CreateBootImageWithSize(rootFile, progPath, commandline string, size DiskSize) error {
	err := createSparseFile(rootFile, size)
	if err != nil {
		return err
	}

	return CreateBootImageOnFile(rootFile, size, progPath, commandline)
}

func CreateBootImageOnFile(rootFile string, sizeOfFile DiskSize, progPath, commandline string) error {

	sizeInSectors, err := ToSectors(sizeOfFile)
	if err != nil {
		return err
	}

	rootLo := NewLoDevice(rootFile)
	rootLodName, err := rootLo.Acquire()
	if err != nil {
		return err
	}
	defer rootLo.Release()

	// use device mapper to rename the lo device to something that grub likes more.
	// like hda!
	grubDiskName := "hda"
	rootBlkDev := NewDevice(0, sizeInSectors, rootLodName, grubDiskName)
	rootDevice, err := rootBlkDev.Acquire()
	if err != nil {
		return err
	}
	defer rootBlkDev.Release()

	p := &MsDosPartioner{rootDevice.Name()}
	p.MakeTable()
	p.MakePartTillEnd("primary", MegaBytes(2))
	parts, err := ListParts(rootDevice)
	if err != nil {
		return err
	}

	if len(parts) < 1 {
		return errors.New("No parts created")
	}

	part := parts[0]
	if dmPart, ok := part.(*DeviceMapperDevice); ok {
		// TODO: is this needed?
		dmPart.DeviceName = grubDiskName + "1"
	}

	// get the block device
	bootDevice, err := part.Acquire()
	if err != nil {
		return err
	}
	defer part.Release()
	bootLabel := "boot"
	// format the device and mount and copy
	err = RunLogCommand("mkfs", "-L", bootLabel, "-I", "128", "-t", "ext2", bootDevice.Name())
	if err != nil {
		return err
	}

	mntPoint, err := Mount(bootDevice)
	if err != nil {
		return err
	}
	defer Umount(mntPoint)

	PrepareGrub(mntPoint, rootDevice.Name(), progPath, commandline)

	err = RunLogCommand("grub-install", "--no-floppy", "--root-directory="+mntPoint, rootDevice.Name())
	if err != nil {
		return err
	}
	return nil
}

func PrepareGrub(folder, rootDeviceName, kernel, commandline string) error {
	grubPath := path.Join(folder, "boot", "grub")
	kernelDst := path.Join(folder, "boot", ProgramName)

	os.MkdirAll(grubPath, 0777)

	// copy program.bin.. skip that for now
	log.WithFields(log.Fields{"src": kernel, "dst": kernelDst}).Debug("copying file")

	if err := CopyFile(kernel, kernelDst); err != nil {
		return err
	}

	if err := writeBootTemplate(path.Join(grubPath, "menu.lst"), "(hd0,0)", commandline); err != nil {
		return err
	}

	if err := writeBootTemplate(path.Join(grubPath, "grub.conf"), "(hd0,0)", commandline); err != nil {
		return err
	}

	if err := writeDeviceMap(path.Join(grubPath, "map"), rootDeviceName); err != nil {
		return err
	}
	return nil
}

func writeDeviceMap(fname, rootDevice string) error {
	f, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer f.Close()

	t := template.Must(template.New("devicemap").Parse(DeviceMapFile))

	log.WithFields(log.Fields{"device": rootDevice, "file": fname}).Debug("Writing device map")
	t.Execute(f, struct {
		GrubDevice string
	}{rootDevice})

	return nil
}
func writeBootTemplate(fname, rootDrive, commandline string) error {

	log.WithFields(log.Fields{"fname": fname, "rootDrive": rootDrive, "commandline": commandline}).Debug("writing boot template")

	f, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer f.Close()

	t := template.Must(template.New("grub").Parse(GrubTemplate))

	t.Execute(f, struct {
		RootDrive   string
		CommandLine string
	}{rootDrive, commandline})

	return nil

}

func formatDeviceAndCopyContents(folder string, dev BlockDevice) error {
	err := RunLogCommand("mkfs", "-I", "128", "-t", "ext2", dev.Name())
	if err != nil {
		return err
	}

	mntPoint, err := Mount(dev)
	if err != nil {
		return err
	}
	defer Umount(mntPoint)

	CopyDir(folder, mntPoint)
	return nil
}

func CreateSingleVolume(rootFile string, folder types.RawVolume) error {
	ext2Overhead := MegaBytes(2).ToBytes()
	size, err := GetDirSize(folder.Path)
	if err != nil {
		return err
	}
	// take a spare sizde and down to sector size
	size = (SectorSize + size + size/10 + int64(ext2Overhead))
	size &^= (SectorSize - 1)
	// 10% buffer.. aligned to 512
	sizeVolume := Bytes(size)
	_, err = ToSectors(Bytes(size))
	if err != nil {
		return err
	}
	err = createSparseFile(rootFile, sizeVolume)
	if err != nil {
		return err
	}

	return CopyToImgFile(folder.Path, rootFile)
}

func CopyToImgFile(folder, imgfile string) error {

	imgLo := NewLoDevice(imgfile)
	imgLodName, err := imgLo.Acquire()
	if err != nil {
		return err
	}
	defer imgLo.Release()

	return formatDeviceAndCopyContents(folder, imgLodName)

}

func copyToPart(folder string, part Resource) error {

	imgLodName, err := part.Acquire()
	if err != nil {
		return err
	}
	defer part.Release()
	return formatDeviceAndCopyContents(folder, imgLodName)

}

func CreatePartitionedVolumes(imgFile string, volumes map[string]types.RawVolume) ([]string, error) {
	sizes := make(map[string]Bytes)
	var orderedKeys []string
	var totalSize Bytes

	ext2Overhead := MegaBytes(2).ToBytes()
	firstPartFffest := MegaBytes(2).ToBytes()

	for mntPoint, localDir := range volumes {
		cursize, err := GetDirSize(localDir.Path)
		if err != nil {
			return nil, err
		}
		sizes[mntPoint] = Bytes(cursize) + ext2Overhead
		totalSize += sizes[mntPoint]
		orderedKeys = append(orderedKeys, mntPoint)
	}
	sizeVolume := Bytes((SectorSize + totalSize + totalSize/10) &^ (SectorSize - 1))
	sizeVolume += MegaBytes(4).ToBytes()

	log.WithFields(log.Fields{"imgFile": imgFile, "size": sizeVolume.ToPartedFormat()}).Debug("Creating image file")
	err := createSparseFile(imgFile, sizeVolume)
	if err != nil {
		return nil, err
	}

	imgLo := NewLoDevice(imgFile)
	imgLodName, err := imgLo.Acquire()
	if err != nil {
		return nil, err
	}
	defer imgLo.Release()

	var p Partitioner
	p = &DiskLabelPartioner{imgLodName.Name()}

	p.MakeTable()
	var start Bytes = firstPartFffest
	for _, mntPoint := range orderedKeys {
		end := start + sizes[mntPoint]
		log.WithFields(log.Fields{"start": start, "end": end}).Debug("Creating partition")
		err := p.MakePart("ext2", start, end)
		if err != nil {
			return nil, err
		}
		curParts, err := ListParts(imgLodName)
		if err != nil {
			return nil, err
		}
		start = curParts[len(curParts)-1].Offset().ToBytes() + curParts[len(curParts)-1].Size().ToBytes()
	}

	parts, err := ListParts(imgLodName)

	log.WithFields(log.Fields{"parts": parts, "volsize": sizes}).Debug("Creating volumes")
	for i, mntPoint := range orderedKeys {
		localDir := volumes[mntPoint].Path

		copyToPart(localDir, parts[i])
	}

	return orderedKeys, nil
}

func CreateVolumes(imgFile string, volumes []types.RawVolume, newPartitioner func(device string) Partitioner) error {

	var sizes []Bytes

	ext2Overhead := MegaBytes(2).ToBytes()
	firstPartOffest := MegaBytes(2).ToBytes()
	var totalSize Bytes = 0
	for _, v := range volumes {
		if v.Size == 0 {
			cursize, err := GetDirSize(v.Path)
			if err != nil {
				return err
			}
			sizes = append(sizes, Bytes(cursize)+ext2Overhead)
		} else {
			sizes = append(sizes, Bytes(v.Size))
		}
		totalSize += sizes[len(sizes)-1]
	}
	sizeDrive := Bytes((SectorSize + totalSize + totalSize/10) &^ (SectorSize - 1))
	sizeDrive += MegaBytes(4).ToBytes()

	log.WithFields(log.Fields{"imgFile": imgFile, "size": totalSize.ToPartedFormat()}).Debug("Creating image file")
	err := createSparseFile(imgFile, sizeDrive)
	if err != nil {
		return err
	}

	imgLo := NewLoDevice(imgFile)
	imgLodName, err := imgLo.Acquire()
	if err != nil {
		return err
	}
	defer imgLo.Release()

	p := newPartitioner(imgLodName.Name())

	p.MakeTable()
	var start Bytes = firstPartOffest
	for _, curSize := range sizes {
		end := start + curSize
		log.WithFields(log.Fields{"start": start, "end": end}).Debug("Creating partition")
		err := p.MakePart("ext2", start, end)
		if err != nil {
			return err
		}
		curParts, err := ListParts(imgLodName)
		if err != nil {
			return err
		}
		start = curParts[len(curParts)-1].Offset().ToBytes() + curParts[len(curParts)-1].Size().ToBytes()
	}

	parts, err := ListParts(imgLodName)

	if len(parts) != len(volumes) {
		return errors.New("Not enough parts created!")
	}

	log.WithFields(log.Fields{"parts": parts, "volsize": sizes}).Debug("Creating volumes")
	for i, v := range volumes {

		copyToPart(v.Path, parts[i])
	}

	return nil
}