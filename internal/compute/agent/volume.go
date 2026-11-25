package agent

import (
	"fmt"
	"os"
	"path/filepath"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
)

// volumeBackend 是卷的存储后端在节点上的操作。各后端建卷、复制和扩容的方式都不同，这些差异只留在实现中；
// Agent 的收敛逻辑只决定"要不要做"。新增后端（ADR-0020 的 LINSTOR）实现这个接口，并在 volumes 中选择。
type volumeBackend interface {
	// Get 返回卷的现状；不存在时 Found 为 false。
	Get(v *computev1alpha1.LibvirtStorageVolume) (Volume, error)
	// Create 创建卷。image 不为空时，内容完整复制自本节点上该镜像的副本。
	Create(v *computev1alpha1.LibvirtStorageVolume, image *computev1alpha1.Image) error
	// Grow 把卷扩大到 capacity 字节。domain 不为空时，它是正在使用这个卷的实例，后端负责让它看到新容量。
	Grow(v *computev1alpha1.LibvirtStorageVolume, cur Volume, capacity uint64, domain string) error
	// Delete 删除卷及其数据；卷不存在时什么也不做。
	Delete(v *computev1alpha1.LibvirtStorageVolume) error
}

// volumes 返回卷所用的后端。本版本只有 spec.libvirt。
func (a *Agent) volumes() volumeBackend { return libvirtVolumes{a.hv} }

// libvirtVolumes 经 libvirt pool 管理卷。
type libvirtVolumes struct{ hv *libvirt.Client }

func (b libvirtVolumes) Get(v *computev1alpha1.LibvirtStorageVolume) (Volume, error) {
	cur, err := b.hv.Volume(v.Spec.Libvirt.Pool, v.Name)
	return Volume{Found: cur.Found, Path: cur.Path, Block: cur.Block, Format: cur.Format, Capacity: cur.Capacity}, err
}

func (b libvirtVolumes) Create(v *computev1alpha1.LibvirtStorageVolume, image *computev1alpha1.Image) error {
	xml, err := prepareVolume(v)
	if err != nil {
		return err
	}
	if image == nil {
		return b.hv.CreateVolume(v.Spec.Libvirt.Pool, xml)
	}
	capacity, err := volumeCapacity(xml)
	if err != nil {
		return err
	}
	if image.Spec.Type == computev1alpha1.ImageRootfs {
		return b.createRootfs(v, image, capacity)
	}
	return b.hv.CreateVolumeFrom(v.Spec.Libvirt.Pool, xml, image.Spec.Pool, image.Name, capacity)
}

// Grow：实例运行中时由 QEMU 在线扩容，因为它持有文件锁，libvirt 改不了正在用的 qcow2；块设备要先扩大
// 设备本身，QEMU 才能看到。libvirt 的 logical pool 不支持改变容量，错误原样写入 status（ADR-0019 §7）。
func (b libvirtVolumes) Grow(v *computev1alpha1.LibvirtStorageVolume, cur Volume, capacity uint64, domain string) error {
	if domain == "" {
		return b.hv.ResizeVolume(v.Spec.Libvirt.Pool, v.Name, capacity)
	}
	if cur.Block && cur.Capacity < capacity {
		if err := b.hv.ResizeVolume(v.Spec.Libvirt.Pool, v.Name, capacity); err != nil {
			return err
		}
	}
	return b.hv.BlockResize(domain, cur.Path, capacity)
}

// createRootfs 在临时文件上生成 ext4，完成后才 rename 成卷：中途崩溃不会留下内容不全、却被当作已有数据
// 接管的卷（ADR-0019 §9）。
func (b libvirtVolumes) createRootfs(v *computev1alpha1.LibvirtStorageVolume, image *computev1alpha1.Image, capacity uint64) error {
	if format := volumeFormat(v.Spec.Libvirt.XML); format != "" && format != formatRaw {
		return fmt.Errorf("a volume from a Rootfs image must be raw, got %q", format)
	}
	dir, err := b.hv.PoolDir(v.Spec.Libvirt.Pool)
	if err != nil {
		return err
	}
	imageDir, err := b.hv.PoolDir(image.Spec.Pool)
	if err != nil {
		return err
	}
	tmp := tempImagePath(dir, v.Name)
	defer func() { _ = os.Remove(tmp) }()
	if err := buildRootfs(tmp, filepath.Join(imageDir, image.Name), capacity); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, v.Name)); err != nil {
		return err
	}
	return b.hv.RefreshPool(v.Spec.Libvirt.Pool)
}

func (b libvirtVolumes) Delete(v *computev1alpha1.LibvirtStorageVolume) error {
	return b.hv.DeleteVolume(v.Spec.Libvirt.Pool, v.Name)
}
