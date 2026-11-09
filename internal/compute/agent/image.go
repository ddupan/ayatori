package agent

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	computev1alpha1 "go.ddupan.top/ayatori/api/compute/v1alpha1"
	"go.ddupan.top/ayatori/internal/compute/adapter/libvirt"
	"go.ddupan.top/ayatori/internal/compute/agent/state"
)

// isoMagicOffset 是 ISO 9660 主卷描述符中 "CD001" 的位置。
const isoMagicOffset = 32769

// download 是后台进行的一次镜像下载。下载只写临时文件、不碰 libvirt，几 GB 的镜像不会卡住
// reconcile（实例的崩溃重启要靠它）；替换副本仍在 reconcile 循环中，与从副本创建卷不会交错。
type download struct {
	sha256 string
	cancel context.CancelFunc
	done   chan struct{}
	err    error // done 关闭后可读
	// lastErr 是上一次尝试的失败原因：重试期间继续显示在 status 中。
	lastErr string
}

func (d *download) finished() bool {
	select {
	case <-d.done:
		return true
	default:
		return false
	}
}

func tempImagePath(dir, name string) string { return filepath.Join(dir, "."+name+".tmp") }

// applyImage 让本节点的副本与 spec 一致。下载完成并校验后 rename 覆盖旧副本，已经打开它的 QEMU 继续
// 读旧的 inode（ADR-0019 §9）。副本的版本记在本地状态的 AppliedInputHash 中。
func (a *Agent) applyImage(r state.Record, img *computev1alpha1.Image) (computev1alpha1.ImageNodeStatus, error) {
	status := computev1alpha1.ImageNodeStatus{NodeName: a.config.NodeName, ObservedGeneration: img.Generation}
	fail := func(err error) (computev1alpha1.ImageNodeStatus, error) {
		status.Message = err.Error()
		return status, err
	}
	dir, err := a.hv.PoolDir(img.Spec.Pool)
	if err != nil {
		return fail(err)
	}
	dst := filepath.Join(dir, img.Name)
	if _, err := os.Stat(dst); err == nil {
		status.SHA256 = r.AppliedInputHash
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	want := img.Spec.Source.HTTP.SHA256
	if status.SHA256 == want {
		a.stopDownload(img.Name)
		status.Ready = true
		return status, nil
	}
	d := a.downloads[img.Name]
	if d == nil || d.sha256 != want {
		a.stopDownload(img.Name)
		a.startDownload(img, tempImagePath(dir, img.Name), "")
		status.Message = "downloading"
		return status, nil
	}
	if !d.finished() {
		status.Message = cmp.Or(d.lastErr, "downloading")
		return status, nil
	}
	if d.err != nil {
		a.startDownload(img, tempImagePath(dir, img.Name), d.err.Error())
		return fail(d.err)
	}
	delete(a.downloads, img.Name)
	if err := os.Rename(tempImagePath(dir, img.Name), dst); err != nil {
		return fail(err)
	}
	if err := a.store.Update(r.Kind, r.Key, func(rec *state.Record) { rec.AppliedInputHash = want }); err != nil {
		return fail(err)
	}
	a.logger.Info("image replaced", "image", img.Name, "sha256", want)
	status.SHA256, status.Ready = want, true
	return status, a.hv.RefreshPool(img.Spec.Pool)
}

func (a *Agent) startDownload(img *computev1alpha1.Image, tmp, lastErr string) {
	// ponytail: 只有总时长上限，连接卡住时要等到超时；需要更快发现时按读取进度判断。
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	d := &download{sha256: img.Spec.Source.HTTP.SHA256, cancel: cancel, done: make(chan struct{}), lastErr: lastErr}
	a.downloads[img.Name] = d
	source, kind := *img.Spec.Source.HTTP, img.Spec.Type
	go func() {
		defer close(d.done)
		defer cancel()
		if d.err = fetchImage(ctx, source, kind, tmp); d.err != nil {
			_ = os.Remove(tmp)
		}
	}()
}

// stopDownload 取消下载并等它退出，之后临时文件不会再被写入。
func (a *Agent) stopDownload(name string) {
	if d := a.downloads[name]; d != nil {
		d.cancel()
		<-d.done
		delete(a.downloads, name)
	}
}

// fetchImage 下载到 tmp，校验内容与类型；出错时由调用方删除 tmp。
func fetchImage(ctx context.Context, source computev1alpha1.ImageHTTPSource, kind computev1alpha1.ImageType, tmp string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", source.URL, resp.Status)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, sum), resp.Body); err != nil {
		return fmt.Errorf("download %s: %w", source.URL, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != source.SHA256 {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, source.SHA256)
	}
	if err := checkImageType(f, kind); err != nil {
		return err
	}
	return f.Sync()
}

// checkImageType 核对内容与 type。raw 磁盘没有文件头，所以 Disk 只能排除 ISO。
func checkImageType(f io.ReaderAt, kind computev1alpha1.ImageType) error {
	magic := make([]byte, 5)
	_, err := f.ReadAt(magic, isoMagicOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	iso := bytes.Equal(magic, []byte("CD001"))
	if iso != (kind == computev1alpha1.ImageISO) {
		return fmt.Errorf("content does not match image type %s (ISO 9660: %t)", kind, iso)
	}
	return nil
}

// localImage 从本地状态取本节点的镜像并核对类型：用错类型时引用方被拒绝。
func (a *Agent) localImage(name string, kind computev1alpha1.ImageType) (state.Record, *computev1alpha1.Image, error) {
	for _, r := range a.store.List(kindImage) {
		if r.Key != name {
			continue
		}
		var img computev1alpha1.Image
		if err := json.Unmarshal(r.Object, &img); err != nil {
			return r, nil, err
		}
		if img.Spec.Type != kind {
			return r, nil, fmt.Errorf("image %s has type %s, need %s", name, img.Spec.Type, kind)
		}
		return r, &img, nil
	}
	return state.Record{}, nil, fmt.Errorf("no such image on this node: %s", name)
}

// createVolumeFromImage 由后端把副本完整复制成新卷。副本与 spec 不一致时
// 等待，不悄悄用旧版本。版本先于创建写入本地状态：创建后崩溃也不会丢。返回复制的版本。
// ponytail: 复制在 reconcile 循环中同步进行，大镜像会推迟其他对象的收敛；成为问题时放到后台。
func (a *Agent) createVolumeFromImage(r state.Record, v *computev1alpha1.LibvirtStorageVolume) (string, error) {
	ir, img, err := a.localImage(v.Spec.Source.Image, computev1alpha1.ImageDisk)
	if err != nil {
		return "", err
	}
	want := img.Spec.Source.HTTP.SHA256
	if ir.Deleted || ir.AppliedInputHash != want {
		return "", fmt.Errorf("image %s not ready on this node: replica %q, want %s", img.Name, ir.AppliedInputHash, want)
	}
	if err := a.store.Update(r.Kind, r.Key, func(rec *state.Record) { rec.AppliedInputHash = want }); err != nil {
		return "", err
	}
	return want, a.volumes().Create(v, img)
}

// resolveDisk 找到 domain 一块盘在本节点的路径。ISO 只要求已有副本：更新在途时已有实例不受影响。
func (a *Agent) resolveDisk(disk computev1alpha1.DomainDisk) (Volume, error) {
	if disk.Volume != "" {
		return a.resolveVolume(disk.Volume)
	}
	r, img, err := a.localImage(disk.Image, computev1alpha1.ImageISO)
	if err != nil {
		return Volume{}, err
	}
	if r.AppliedInputHash == "" {
		return Volume{}, errors.New("image not downloaded yet")
	}
	dir, err := a.hv.PoolDir(img.Spec.Pool)
	return Volume{Path: filepath.Join(dir, img.Name)}, err
}

// removeImage 删除本节点的副本；仍有实例（包括待清理的）挂载时延后，与卷的本地保护相同。
func (a *Agent) removeImage(r state.Record) (bool, error) {
	var img computev1alpha1.Image
	if err := json.Unmarshal(r.Object, &img); err != nil {
		return false, err
	}
	for _, d := range a.store.List(kindDomain) {
		var dom computev1alpha1.LibvirtDomain
		if err := json.Unmarshal(d.Object, &dom); err != nil {
			return false, err
		}
		for _, disk := range dom.Spec.Disks {
			if disk.Image == img.Name {
				a.logger.Info("image still mounted, deleting later", "key", r.Key, "domain", d.Key)
				return false, nil
			}
		}
	}
	a.stopDownload(img.Name)
	dir, err := a.hv.PoolDir(img.Spec.Pool)
	if libvirt.IsNotFound(err) && r.AppliedInputHash == "" {
		return true, nil // pool 从未就绪，也就没有写过副本
	} else if err != nil {
		return false, err
	}
	for _, path := range []string{filepath.Join(dir, img.Name), tempImagePath(dir, img.Name)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	return true, a.hv.RefreshPool(img.Spec.Pool)
}
