// Package cloudinit 是实例的 cloud-init NoCloud seed（ADR-0016）：VM 经元数据服务以 HTTP 读取，LXC 经只读挂载。
package cloudinit

// SeedURL 是元数据服务上的 NoCloud seed，cloud-init 在其后拼接 meta-data、user-data 等文件名。
const SeedURL = "http://169.254.169.254/nocloud/"

// SMBIOSSerial 写在 VM 的 SMBIOS system serial 中，cloud-init 的 ds-identify 据此选用 NoCloud 并从 SeedURL 读取。
const SMBIOSSerial = "ds=nocloud;s=" + SeedURL

// MetaData 是实例的 NoCloud meta-data。instance-id 取对象 UID：重建同名对象时 cloud-init 视为新实例。
func MetaData(uid, hostname string) string {
	return "instance-id: " + uid + "\nlocal-hostname: " + hostname + "\n"
}
