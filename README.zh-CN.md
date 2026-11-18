# mcp-yt-dlp

[English](README.md) | 简体中文

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[OpenPlayX](https://github.com/openplayx/openplayx) 的外部服务，把 [yt-dlp](https://github.com/yt-dlp/yt-dlp) 封装成 MCP 服务：先查看视频链接的信息，再按选定的画质在服务器后台下载。服务是一个 Go 程序，调用固定版本的 yt-dlp 可执行文件；客户端无法直接传入 yt-dlp 参数。

- MCP 走 Streamable HTTP，端点 `POST /mcp`（无状态 JSON-RPC，Bearer token），与其他 OpenPlayX MCP 服务支持的子集相同。OpenPlayX 会从服务根地址探测 `/mcp`，所以 App 里只需填根地址和 token。
- `/healthz`（存活检查）和 `/readyz`（下载目录可写、yt-dlp、ffmpeg 和 JS 运行时就绪）不需要 token。

## 工具

| 工具 | 权限范围 | 只读 | 作用 |
|---|---|---|---|
| `get_status` | read | 是 | 空闲空间、限制、预设、任务数量 |
| `get_media_info` | read | 是 | 标题、时长、分辨率、字幕语言、各预设的预估大小；播放列表链接则返回列表条目 |
| `start_download` | download | 否 | 把一个视频加入队列（`url`、`preset`、`subtitles`）；先检查大小、时长和空闲空间；返回任务 |
| `list_downloads` | read | 是 | 最近的任务，可按状态筛选 |
| `get_download` | read | 是 | 单个任务的进度、生成的文件和错误 |
| `cancel_download` | download | 否 | 停止排队中/运行中的任务，丢弃未完成的文件 |
| `remove_download` | download | 否（破坏性） | 删除已结束任务的记录；`delete_files: true` 时同时删除文件 |

所有非只读工具，OpenPlayX 都会先请用户确认。

预设：`best`、`2160p`、`1080p`（默认）、`720p`、`480p`（限制最大高度，编码允许时合并为 mp4，否则为 mkv），以及 `audio`（m4a）。字幕以 `.srt` 保存在视频旁边。客户端不能直接传 yt-dlp 的原始参数。

有意不支持的功能：整个播放列表（每个条目链接单独下载）、直播、cookies/登录、`yt-dlp -U` 自更新。

### 下载进度

下载过程中，`progress.scope: "current_stream"` 和从 1 开始的 `stream_index`
标识当前第几路流（视频、音频、字幕按首次出现顺序编号，总路数未知）。
`downloaded_bytes`、`total_bytes`（兼容字段 `current_total_bytes` 返回同一值）、
`percent`（0–100）和 `eta_seconds` 均对应当前流。单路达到 100% 不代表任务完成，
仍需检查 `state` 和 `phase`。`overall_downloaded_bytes` 单独累计各路流的下载字节，
用于检查任务大小上限。

`total_bytes_estimated: true` 时应将大小显示为“约”；分片估算值以及对应的百分比和 ETA
可能变化。总大小未知或小于已下载字节时，总大小、百分比和 ETA 返回 null。
ETA 按剩余字节 / `speed_bps` 向上取整，并标注 `eta_estimated: true`；
无有效速度或处于后处理阶段时 ETA 为 null。任务完成后返回累计传输字节、精确总大小和
100%；合并后的输出大小在 `files` 中，可能与传输字节不同。
任务顶层的 `estimated_bytes` 仅是初始探测估算，不应作为下载进度的计算依据。

## 限制与安全

- 单任务大小上限（`max_task_bytes`）：入队前按预估大小检查，下载过程中按实际写入的字节数检查。
- 空闲空间保留（`min_free_bytes`）：入队前检查，下载过程中每 2 秒检查一次。
- 只接受公网 http(s) 链接（不能带凭据，除非开启 `allow_private_addresses`，否则不允许私有/回环地址）。yt-dlp 自己会跟随重定向并请求其他地址；可以设置 `allow_generic_extractor: false` 或 `allowed_extractors`，把范围限制在已知网站。
- 重启后不会恢复执行任务：排队中/运行中的任务变为 `interrupted`，未完成的文件被删除。

## 快速开始

依赖：Docker。本地开发需要 Go 1.26；要在本机运行服务，`PATH` 里还要有 yt-dlp、ffmpeg 和 deno。

1. 生成 token 和配置：

   ```bash
   cd deploy
   cp config.example.json config.json
   cp secrets.example.json secrets.json
   TOKEN=$(openssl rand -hex 32)
   printf '%s' "$TOKEN" | shasum -a 256   # 把摘要填进 secrets.json 的 token_sha256
   ```

   也可以不用摘要，直接写 `"token": "<明文 token>"`。

2. 构建并启动：

   ```bash
   docker build -t openplayx/yt-dlp:local ..
   mkdir -p ../data ../downloads
   YTDLP_IMAGE=openplayx/yt-dlp:local APP_UID=$(id -u) APP_GID=$(id -g) \
     DATA_DIR=../data DOWNLOAD_ROOT=../downloads docker compose up -d
   ```

   服务监听 `127.0.0.1:18800`（用 `BIND` 和 `PORT` 修改）；下载的文件保存在 `downloads/`。

3. 检查：

   ```bash
   curl -s http://127.0.0.1:18800/readyz
   curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_status","arguments":{}}}' \
     http://127.0.0.1:18800/mcp
   ```

如果要在本机或局域网以外使用，请在前面加一层 HTTPS 反向代理。

## 配置

| 文件 | 内容 |
|---|---|
| `config.json`（[示例](deploy/config.example.json)） | 监听地址、存储路径、限制（`max_task_bytes`、`min_free_bytes`、`max_duration_seconds`、并发数、限速）、网络策略（`allow_private_addresses`、`allowed_extractors`、`allow_generic_extractor`、`proxy_ref`） |
| `secrets.json`（[示例](deploy/secrets.example.json)） | token（名称、`token` 或 `token_sha256`、权限范围 `read` / `download`），以及 `proxy_ref` 引用的可选代理地址 |

`secrets.json` 和 `config.json` 已被 git 忽略。

## 在 MCP 客户端中使用

任何支持 Streamable HTTP 和 Bearer token 的客户端都可以。例如：

```json
{
  "mcpServers": {
    "yt-dlp": {
      "type": "http",
      "url": "http://127.0.0.1:18800/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

在 OpenPlayX 中，到 **AI → 外部能力** 添加，填服务根地址和 token。

## 开发

```bash
scripts/check.sh        # gofmt、go vet、测试（模拟引擎，不联网）
YTDLP_BIN=$(command -v yt-dlp) scripts/check.sh   # 另外用真实 yt-dlp 跑一次本地集成测试
go run ./src/cmd/mcp-yt-dlp --config deploy/config.json --secrets deploy/secrets.json
```

`YTDLP_BIN` 指定 yt-dlp 可执行文件（默认用 `PATH` 里的 `yt-dlp`）。

yt-dlp 的版本和升级步骤见 [UPSTREAM.md](UPSTREAM.md)（英文）。

## 从 Python 服务迁移

重新构建镜像并重建容器，沿用现有 Compose 文件、配置、token、数据目录和下载目录。
MCP 工具、端口和 SQLite 任务数据结构保持兼容。任务记录和已完成的文件会保留；
排队中或运行中的任务在重启后变为 `interrupted`，未完成的文件会被清理，需要手动重新提交下载。

Go 服务通过子进程调用 yt-dlp。本地部署需要 Go 和上面列出的外部工具，Docker 镜像已包含这些依赖。
文件收集只包含已完成的输出及其带语言标签的附属文件，因此新任务不会再把另一预设的输出列为附属文件或一并删除。

## 参与贡献

欢迎提 issue 和 PR，见 [CONTRIBUTING.md](CONTRIBUTING.md)（英文）。安全问题请按 [SECURITY.md](SECURITY.md)（英文）私下报告。

## 声明

下载什么内容由你自己负责。只下载你有权下载的内容，并遵守相关网站的服务条款以及适用于你的版权法律。本项目不托管、不分发任何媒体，与 yt-dlp 及任何网站都没有关联。

## 许可证

[MIT](LICENSE)。yt-dlp 是独立的程序，采用 The Unlicense；Docker 镜像中的 ffmpeg 和 deno 遵循各自的许可证。
