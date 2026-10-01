package credential

// Location 是与 Database 生命周期一致的凭据定位，不是密码。
type Location struct {
	Mount string
	Path  string
}
