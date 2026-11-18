# pan

一个基于百度网盘官方开放 API 的递归下载 CLI。支持目录递归、并发下载、断点续传、
任务状态持久化、失败重试、OAuth token 自动刷新和可选 MD5 校验。

## 准备

1. 在[百度网盘开放平台](https://pan.baidu.com/union/)创建应用，获得 API Key 和 Secret Key。
2. 在应用后台配置 OAuth 回调地址，并确保应用已获得网盘文件读取/下载能力。
3. 安装 Go 1.23+，编译：

```bash
go build -o pan ./cmd/pan
```

## 配置

所有配置都在项目根目录的 `conf.yaml`：

```yaml
app:
  app_id: "应用 ID"
  app_key: "应用 API Key"
  secret_key: "应用 Secret Key"
  sign_key: "应用 Sign Key"
oauth:
  redirect_uri: "应用后台配置的回调地址"
download:
  remote_paths:
    - "/"
  output: "./downloads"
  workers: 3
  retries: 5
  verify_md5: false
token:
  access_token: ""
  refresh_token: ""
  expires_at: "0001-01-01T00:00:00Z"
```

`token` 会在授权及后续自动刷新时写回，无需手工填写。`conf.yaml` 包含密钥，已加入
`.gitignore`，文件权限应保持为 `0600`。

## 授权

先生成授权地址：

```bash
./pan auth
```

浏览器授权后，从跳转地址复制 `code`：

```bash
./pan auth --code '回调中的 code'
```

也可以通过 `--config` 或 `PAN_CONFIG` 使用其他 YAML 配置文件。

## 下载与续传

下载整个网盘：

```bash
./pan download
```

命令被 Ctrl-C、断网或进程退出打断后，重新运行完全相同的命令即可续传。
`resume` 是 `download` 的别名。部分文件保存在 `<文件名>.part`；任务状态保存在
`<输出目录>/.pan-downloader/state.json`，每秒更新且采用原子替换写入。

查看进度：

```bash
./pan status
```

完整性要求较高时可增加 `--verify-md5`。该选项会在下载完成后额外读取一遍文件。

## 注意

- 本工具只使用官方开放 API，不支持下载未授权的分享链接，也不会绕过会员限速。
- 并发数过高可能触发接口限流，建议从 2–3 开始。
- 不要提交 OAuth 配置文件或输出目录中的状态文件；两者可能包含敏感路径或凭据。

