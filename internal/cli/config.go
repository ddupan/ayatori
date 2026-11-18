// Package cli 实现 ayatori CLI 的配置、发现与登录（ADR-0027）。命令行本身在 cmd/ayatori。
package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"sigs.k8s.io/yaml"
)

// Profile 是一个环境：连哪个 apiserver、信任哪个 CA、去哪里登录。由 `ayatori config add` 写入。
type Profile struct {
	Server string `json:"server"`
	// CertificateAuthority 是用户确认过指纹的集群 CA（PEM）。
	CertificateAuthority string `json:"certificateAuthority"`
	Issuer               string `json:"issuer"`
	ClientID             string `json:"clientID"`
}

// Config 是 CLI 自己的配置文件，不是 kubeconfig。
type Config struct {
	Current  string             `json:"current,omitempty"`
	Profiles map[string]Profile `json:"profiles,omitempty"`
}

// Store 是配置目录：config.yaml，以及 tokens/ 下每个环境一份只有本人可读的 token。
type Store struct{ Dir string }

// DefaultStore 是 ~/.ayatori。
func DefaultStore() (Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Store{}, err
	}
	return Store{Dir: filepath.Join(home, ".ayatori")}, nil
}

func (s Store) Load() (Config, error) {
	var c Config
	data, err := os.ReadFile(filepath.Join(s.Dir, "config.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, yaml.UnmarshalStrict(data, &c)
}

func (s Store) Save(c Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(s.Dir, "config.yaml"), data)
}

// Profile 返回指定的环境；name 为空时用当前环境。
func (c Config) Profile(name string) (string, Profile, error) {
	if name == "" {
		name = c.Current
	}
	if name == "" {
		return "", Profile{}, errors.New("no environment selected: run `ayatori config add` or `ayatori config use`")
	}
	p, ok := c.Profiles[name]
	if !ok {
		return "", Profile{}, fmt.Errorf("unknown environment %q (see `ayatori config list`)", name)
	}
	return name, p, nil
}

func (c Config) Names() []string {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// writeFile 先写临时文件再改名，进程中途退出也不会留下半个文件。目录与文件都只有本人可访问。
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
