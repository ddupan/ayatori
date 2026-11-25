// ayatori 是人使用 Ayatori 的命令行（ADR-0027）。环境由 `ayatori config` 管理，写在 CLI 自己的
// 配置文件里，不用 kubeconfig。
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/rest"

	"go.ddupan.top/ayatori/internal/cli"
)

// version 由发布构建以 -ldflags "-X main.version=<tag>" 写入；本地构建是 dev。
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := root().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	var profile string
	cmd := &cobra.Command{Use: "ayatori", Short: "Ayatori command line", Version: version,
		SilenceUsage: true, SilenceErrors: true}
	cmd.PersistentFlags().StringVar(&profile, "profile", "", "environment to use instead of the current one")
	cmd.AddCommand(configCmd(&profile), loginCmd(&profile), logoutCmd(&profile), whoamiCmd(&profile), versionCmd(&profile))
	// 名词在前、动词在后（ADR-0027），参照云 CLI，不是 kubectl 的动词在前。
	for _, r := range cli.Resources {
		cmd.AddCommand(resourceCmd(r, &profile))
	}
	return cmd
}

func store() (cli.Store, cli.Config, error) {
	s, err := cli.DefaultStore()
	if err != nil {
		return s, cli.Config{}, err
	}
	c, err := s.Load()
	return s, c, err
}

func configCmd(profile *string) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Manage the environments this CLI connects to"}

	var fingerprint string
	add := &cobra.Command{
		Use:   "add NAME SERVER",
		Short: "Add an environment, trusting its CA after you confirm the fingerprint",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, c, err := store()
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := c.Profiles[name]; exists {
				return fmt.Errorf("environment %q exists; remove it first", name)
			}
			p, err := cli.Discover(cmd.Context(), args[1], func(ca cli.CA) error {
				return confirmCA(cmd, ca, fingerprint)
			})
			if err != nil {
				return err
			}
			if c.Profiles == nil {
				c.Profiles = map[string]cli.Profile{}
			}
			c.Profiles[name] = p
			if c.Current == "" {
				c.Current = name
			}
			if err := s.Save(c); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Added %s (%s). Run `ayatori login%s` to sign in.\n",
				name, p.Server, profileFlag(c, name))
			return nil
		},
	}
	add.Flags().StringVar(&fingerprint, "ca-fingerprint", "", "expected CA fingerprint (sha256:...), instead of asking")

	list := &cobra.Command{
		Use:   "list",
		Short: "List environments",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, c, err := store()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "CURRENT\tNAME\tSERVER")
			for _, name := range c.Names() {
				mark := ""
				if name == c.Current {
					mark = "*"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", mark, name, c.Profiles[name].Server)
			}
			return w.Flush()
		},
	}

	use := &cobra.Command{
		Use:   "use NAME",
		Short: "Make NAME the current environment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, c, err := store()
			if err != nil {
				return err
			}
			if _, _, err := c.Profile(args[0]); err != nil {
				return err
			}
			c.Current = args[0]
			return s.Save(c)
		},
	}

	remove := &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove an environment and its saved login",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, c, err := store()
			if err != nil {
				return err
			}
			if _, _, err := c.Profile(args[0]); err != nil {
				return err
			}
			if err := s.Logout(args[0]); err != nil {
				return err
			}
			delete(c.Profiles, args[0])
			if c.Current == args[0] {
				c.Current = ""
			}
			return s.Save(c)
		},
	}
	kubeconfig := &cobra.Command{
		Use:   "kubeconfig",
		Short: "Print a kubeconfig for break-glass kubectl access, using this CLI's login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, c, err := store()
			if err != nil {
				return err
			}
			name, p, err := c.Profile(*profile)
			if err != nil {
				return err
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			data, err := cli.Kubeconfig(name, p, self)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}

	// credential 由 kubeconfig 的 exec 调用，不是给人用的。
	credential := &cobra.Command{
		Use:    "credential",
		Short:  "Print an ExecCredential for kubectl",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, c, err := store()
			if err != nil {
				return err
			}
			name, p, err := c.Profile(*profile)
			if err != nil {
				return err
			}
			data, err := s.ExecCredential(cmd.Context(), name, p)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	cmd.AddCommand(add, list, use, remove, kubeconfig, credential)
	return cmd
}

func confirmCA(cmd *cobra.Command, ca cli.CA, expected string) error {
	if expected != "" {
		if !strings.EqualFold(expected, ca.Fingerprint) {
			return fmt.Errorf("server CA fingerprint %s does not match --ca-fingerprint", ca.Fingerprint)
		}
		return nil
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Server CA: %s (%s)\n", ca.Fingerprint, ca.Subject)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("not a terminal: pass --ca-fingerprint to trust this CA")
	}
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), "Trust this CA? [y/N] ")
	answer, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if a := strings.TrimSpace(strings.ToLower(answer)); a != "y" && a != "yes" {
		return errors.New("CA not trusted")
	}
	return nil
}

func profileFlag(c cli.Config, name string) string {
	if c.Current == name {
		return ""
	}
	return " --profile " + name
}

func loginCmd(profile *string) *cobra.Command {
	var device bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to the current environment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, c, err := store()
			if err != nil {
				return err
			}
			name, p, err := c.Profile(*profile)
			if err != nil {
				return err
			}
			opts := cli.LoginOptions{Device: device, Out: cmd.ErrOrStderr(), Open: openBrowser}
			if err := s.Login(cmd.Context(), name, p, opts); err != nil {
				return err
			}
			config, err := s.RESTConfig(cmd.Context(), name, p)
			if err != nil {
				return err
			}
			user, err := cli.WhoAmI(cmd.Context(), config)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s as %s\n", name, user.Username)
			// 登录时检查版本偏差（ADR-0027），只警告；读不到版本不影响登录。
			if v, err := cli.ServerVersion(cmd.Context(), config); err == nil {
				if warning := cli.SkewWarning(version, v.Ayatori); warning != "" {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), warning)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&device, "device", false, "sign in on another device with a code (no local browser)")
	return cmd
}

func logoutCmd(profile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the saved login for the current environment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, c, err := store()
			if err != nil {
				return err
			}
			name, _, err := c.Profile(*profile)
			if err != nil {
				return err
			}
			return s.Logout(name)
		},
	}
}

// versionCmd 与 kubectl version 相同：先打印客户端，再连接环境打印服务端，并检查版本偏差。没有配置
// 环境时只打印客户端。
func versionCmd(profile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the CLI and server versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintf(w, "Client\t%s\nclient-go\t%s\n", version, cli.ClientGoVersion())
			s, c, err := store()
			if err != nil {
				return err
			}
			if c.Current == "" && *profile == "" {
				return w.Flush()
			}
			name, p, err := c.Profile(*profile)
			if err != nil {
				return err
			}
			config, err := s.RESTConfig(cmd.Context(), name, p)
			if err != nil {
				_ = w.Flush()
				return err
			}
			v, err := cli.ServerVersion(cmd.Context(), config)
			if err != nil {
				_ = w.Flush()
				return err
			}
			server := v.Ayatori
			if server == "" {
				server = "unknown (the server does not publish its version)"
			}
			_, _ = fmt.Fprintf(w, "Server (%s)\t%s\nKubernetes\t%s\n", name, server, v.Kubernetes)
			if err := w.Flush(); err != nil {
				return err
			}
			if warning := cli.SkewWarning(version, v.Ayatori); warning != "" {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), warning)
			}
			return nil
		},
	}
}

func whoamiCmd(profile *string) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who the server says you are",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, c, err := store()
			if err != nil {
				return err
			}
			name, p, err := c.Profile(*profile)
			if err != nil {
				return err
			}
			config, err := s.RESTConfig(cmd.Context(), name, p)
			if err != nil {
				return err
			}
			user, err := cli.WhoAmI(cmd.Context(), config)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintf(w, "Environment\t%s (%s)\nUsername\t%s\nGroups\t%s\n", name, p.Server,
				user.Username, strings.Join(user.Groups, ", "))
			return w.Flush()
		},
	}
}

func openBrowser(url string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, url) //nolint:gosec // 打开 issuer 给出的登录地址
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Start()
}

// connect 以当前（或 --profile 指定的）环境与已保存的登录连接 apiserver。
func connect(ctx context.Context, profile string) (*rest.Config, error) {
	s, c, err := store()
	if err != nil {
		return nil, err
	}
	name, p, err := c.Profile(profile)
	if err != nil {
		return nil, err
	}
	return s.RESTConfig(ctx, name, p)
}

func resourceCmd(r cli.Resource, profile *string) *cobra.Command {
	var namespace string
	var all bool
	cmd := &cobra.Command{Use: r.Noun, Short: r.Short}
	cmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "namespace")

	var output string
	list := &cobra.Command{
		Use:   "list [NAME]",
		Short: "List " + r.Short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := connect(cmd.Context(), *profile)
			if err != nil {
				return err
			}
			opts := cli.ListOptions{Namespace: namespace, Output: output}
			if all {
				opts.Namespace = ""
			}
			if len(args) == 1 {
				opts.Name = args[0]
			}
			return cli.List(cmd.Context(), config, r, opts, cmd.OutOrStdout())
		},
	}
	list.Flags().BoolVarP(&all, "all-namespaces", "A", false, "list across all namespaces")
	list.Flags().StringVarP(&output, "output", "o", "", "wide, json, yaml or jsonpath=<template>")

	describe := &cobra.Command{
		Use:   "describe NAME",
		Short: "Show one of the " + r.Short + " with its conditions and events",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := connect(cmd.Context(), *profile)
			if err != nil {
				return err
			}
			return cli.Describe(cmd.Context(), config, r, namespace, args[0], cmd.OutOrStdout())
		},
	}
	cmd.AddCommand(list, describe)
	return cmd
}
