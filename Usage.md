# MAM-Go 系统使用文档

## 1. 系统简介

MAM-Go 是一个轻量级、高性能的私有化媒资管理系统，专为影视制作小团队设计。

* **服务端 (Server)**：负责素材存储、Web 审阅、元数据管理及 WebDAV 归档。
* **客户端 (Client)**：利用 macOS 硬件加速 (VideoToolbox) 自动转码生成 Proxy，提取元数据并断点续传至服务器。

---

## 2. 环境准备

在开始部署前，请确保满足以下条件：

### 通用依赖

* **Go 语言环境**: 1.20 或更高版本。
* **网络**: 服务端与客户端需在同一局域网，或服务端有公网 IP。

### 服务端 (Server) 要求

* **操作系统**: Linux (推荐) / macOS / Windows。

### 客户端 (Client) 要求

* **操作系统**: **macOS** (强烈推荐，代码深度绑定 `hevc_videotoolbox` 硬件加速)。
* **FFmpeg**: 必须安装 FFmpeg 且支持 `hevc_videotoolbox` 编码器。
```bash
brew install ffmpeg

```



---

## 3. 服务端 (Server) 部署指南

服务端负责接收素材并提供审阅界面。

### 3.1 编译与安装

进入 `server` 目录进行编译：

```bash
cd server
go mod tidy
go build -o mam-server .

```

编译成功后会生成可执行文件 `mam-server` (Windows 为 `mam-server.exe`)。

### 3.2 配置文件 (.env)

首次运行服务端时，如果找不到配置文件，程序会自动生成一个默认的 `.env` 文件并退出。

您可以手动创建或修改 `.env` 文件，详细配置如下：

```ini
# --- 服务器基础配置 ---
SERVER_PORT=:8080            # 监听端口
ADMIN_PASSWORD=my_secret_pwd # 管理员后台密码 (Admin 用户名默认为 admin)
UPLOAD_ROOT=storage          # 素材存储根目录
DB_PATH=mam.db               # SQLite 数据库路径

# --- 自动化运维配置 ---
ENABLE_WEBDAV=true           # 是否开启 WebDAV 自动备份
WEBDAV_URL=http://127.0.0.1:5244/dav/ # WebDAV 地址
WEBDAV_USER=admin            # WebDAV 用户名
WEBDAV_PASS=admin            # WebDAV 密码

ENABLE_AUTO_CLEAN=true       # 是否开启旧版本自动清理
KEEP_VERSIONS=5              # 保留最近多少个版本
SAFE_CLEAN_MODE=true         # 安全模式：仅清理已备份(backup_status=1)的文件

# --- 通知配置 ---
GOTIFY_URL=http://127.0.0.1:8081/message # Gotify 通知服务器地址
GOTIFY_TOKEN=token_here      # Gotify Token

```

### 3.3 启动服务

指定配置文件启动服务端：

```bash
./mam-server -env .env

```

启动成功后，控制台会显示 `Server starting on :8080`。

### 3.4 Web 界面功能

* **管理后台**: 访问 `http://localhost:8080/admin`
* 登录账号: `admin` / 配置文件中的密码。
* 功能: 创建项目、查看项目列表、手动上传 Demo 视频、生成分享链接 (Reviewer/Viewer)。


* **审阅页面**: 通过管理后台生成的 Token 链接访问 (如 `/watch?token=...`)。
* 功能: 观看样片、逐帧评论、标记 Good Take。



---

## 4. 客户端 (Client) 使用指南

客户端通常部署在拍摄现场的 DIT 工作站 (Mac Mini/MacBook) 上。

### 4.1 编译客户端

进入 `client` 目录：

```bash
cd client
go mod tidy
go build -o ingest_client .

```

### 4.2 命令行参数说明

客户端通过命令行参数控制行为：

| 参数 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `-project` | **是** | 无 | 项目名称，将作为服务端的一级目录分类。 |
| `-server` | 否 | `http://localhost:8080` | 服务端地址 (末尾不要带斜杠)。 |
| `-source` | 否 | `.` (当前目录) | 素材源目录，客户端会递归扫描该目录。 |

### 4.3 常用操作示例

**场景 1：基础上传**
将当前目录下的素材上传到名为 "Documentary_Day1" 的项目中，服务端在本地。

```bash
./ingest_client -project "Documentary_Day1"

```

**场景 2：指定素材卡路径和服务端 IP**
读取外置存储卡 `/Volumes/CardA`，上传到远程服务器 `192.168.1.100`。

```bash
./ingest_client -project "Commercial_Nike" -source "/Volumes/CardA" -server "http://192.168.1.100:8080"

```

### 4.4 客户端运行逻辑

1. **扫描**: 递归扫描 `-source` 目录下的 `.mov`, `.mp4`, `.mxf` 文件。
2. **防休眠**: 自动调用 macOS `caffeinate` 防止系统在传输过程中休眠。
3. **转码**: 调用 `ffmpeg` 使用 `hevc_videotoolbox` 硬件加速，生成 1080p H.265 Proxy 文件。
4. **校验**: 计算 Proxy 文件的 MD5 哈希值。
5. **上传**:
* 支持断点续传：自动检测服务端已上传的大小。
* 智能跳过：如果服务端已存在完整文件，则自动跳过。
* 元数据同步：自动提取原始素材的 Metadata (如相机型号、分辨率) 上传至服务器。



---

## 5. 完整工作流示例

1. **服务端准备**:
* 在机房或云服务器启动 `mam-server`。
* 登录后台 `http://server-ip:8080/admin` 创建项目 "MyFilm"。


2. **现场 DIT 工作**:
* 将拍摄素材从存储卡复制到 DIT 电脑 (Mac) 的 `/Data/Day01` 目录。
* 运行客户端:
```bash
./ingest_client -server "http://server-ip:8080" -project "MyFilm" -source "/Data/Day01"

```


* 客户端开始高速转码并上传。


3. **异地审阅**:
* 制片人或导演打开服务端分享的 "Dailies Link" (样片链接)。
* 在网页端在线浏览当天拍摄的素材，对满意的镜头点击 "Star" (标记 Good Take)。
* 如果上传的是剪辑版本 (Demo)，审阅人可以在时间线上发布精确到帧的修改意见。



---

## 6. 注意事项与故障排查

* **转码失败**: 客户端硬编码了 `-c:v hevc_videotoolbox`。如果在非 macOS 系统或未安装对应编码器的环境中运行，会报错。如需在 Linux/Windows 运行，请修改 `client/ingest_client.go` 中的 ffmpeg 参数（例如改为 `libx264` 或 `nvenc`）。
* **断点续传**: 如果上传中断，再次运行相同的命令即可。客户端会通过 API `/api/dailies/check` 检查进度并从断点处继续上传。
* **WebDAV 备份**: 服务端配置了每日凌晨 (默认逻辑，具体看调度器实现) 或定时任务将素材备份到 WebDAV 网盘。请确保 `.env` 中的 WebDAV 配置正确，否则通过 WebDAV 备份会失败，但不会影响本地存储。