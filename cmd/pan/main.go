package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"example.com/pan/internal/baidu"
	"example.com/pan/internal/downloader"
	"example.com/pan/internal/state"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "auth":
		return auth(args[1:])
	case "download", "resume":
		return download(args[1:])
	case "status":
		return status(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("未知命令 %q（运行 pan help 查看帮助）", args[0])
	}
}

func usage() {
	fmt.Print(`pan - 可恢复的百度网盘递归下载器

用法:
  pan auth [--config conf.yaml]
  pan auth --code CODE [--config conf.yaml]
  pan download [--config conf.yaml]
  pan status [--config conf.yaml]

所有配置均从 conf.yaml 读取。第一次 auth 不传 --code 会打印授权地址；在浏览器
授权后，把回调地址中的 code 传给第二次 auth。download 和 resume 等价。
`)
}

func auth(args []string) error {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	code := fs.String("code", "", "授权回调中的 code")
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := state.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	if cfg.OAuth.RedirectURI == "" {
		return fmt.Errorf("conf.yaml 中 oauth.redirect_uri 不能为空")
	}
	if *code == "" {
		fmt.Println("请在浏览器打开以下地址完成授权：")
		fmt.Println(baidu.AuthorizationURL(cfg.App.AppKey, cfg.OAuth.RedirectURI))
		fmt.Println("\n授权后再次运行本命令，并增加 --code 回调地址中的值。")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := baidu.ExchangeCode(ctx, nil, cfg.App.AppKey, cfg.App.SecretKey, cfg.OAuth.RedirectURI, *code)
	if err != nil {
		return err
	}
	cfg.Token = *tok
	if err := state.SaveConfig(*configPath, cfg); err != nil {
		return err
	}
	fmt.Printf("授权成功，凭据已保存到 %s\n", *configPath)
	return nil
}

func download(args []string) error {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := state.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	if cfg.Token.AccessToken == "" {
		return fmt.Errorf("尚未授权，请先运行 pan auth")
	}
	if cfg.Download.Workers < 1 || cfg.Download.Workers > 32 {
		return fmt.Errorf("conf.yaml 中 download.workers 必须在 1 到 32 之间")
	}
	if len(cfg.Download.RemotePaths) == 0 {
		cfg.Download.RemotePaths = []string{"/"}
	}
	if cfg.Download.Output == "" {
		return fmt.Errorf("conf.yaml 中 download.output 不能为空")
	}
	absOutput, err := filepath.Abs(cfg.Download.Output)
	if err != nil {
		return err
	}
	store, err := state.Open(filepath.Join(absOutput, ".pan-downloader", "state.json"))
	if err != nil {
		return err
	}
	client := baidu.NewClient(cfg.App.AppKey, cfg.App.SecretKey, cfg.Token, func(tok baidu.Token) error {
		cfg.Token = tok
		return state.SaveConfig(*configPath, cfg)
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	roots := normalizePaths(cfg.Download.RemotePaths)
	d := downloader.New(client, store, downloader.Options{
		Output: absOutput, Workers: cfg.Download.Workers, Retries: cfg.Download.Retries, VerifyMD5: cfg.Download.VerifyMD5,
	})
	return d.Run(ctx, roots)
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := state.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	abs, err := filepath.Abs(cfg.Download.Output)
	if err != nil {
		return err
	}
	store, err := state.Open(filepath.Join(abs, ".pan-downloader", "state.json"))
	if err != nil {
		return err
	}
	s := store.Summary()
	fmt.Printf("文件: %d，已完成: %d，下载中/待下载: %d，失败: %d\n", s.Total, s.Completed, s.Pending, s.Failed)
	fmt.Printf("进度: %s / %s (%.1f%%)\n", humanBytes(s.Downloaded), humanBytes(s.Bytes), s.Percent())
	for _, f := range store.Failed() {
		fmt.Printf("失败  %s: %s\n", f.RemotePath, f.Error)
	}
	return nil
}

func defaultConfigPath() string {
	if p := os.Getenv("PAN_CONFIG"); p != "" {
		return p
	}
	return "conf.yaml"
}

func normalizePaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, item := range paths {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.HasPrefix(item, "/") {
			item = "/" + item
		}
		if item != "/" {
			item = strings.TrimRight(item, "/")
		}
		result = append(result, item)
	}
	if len(result) == 0 {
		return []string{"/"}
	}
	return result
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
