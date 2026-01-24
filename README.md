# MAM-Go 媒资管理系统

基于 Go 语言开发的高性能媒资管理系统 (MAM)，包含服务端 (Server) 和 客户端 (Client)。专为影视制作流程设计，支持 H.265 硬件转码、断点续传、元数据提取及数据完整性校验。适合学生、以及小团队使用。

## 系统架构

*   **Server (服务端)**: 基于 Gin 框架 + SQLite 数据库。负责元数据存储、文件接收、Web 预览及 WebDAV 归档。
*   **Client (客户端)**: 推荐运行在 Mac Mini 等高性能终端。负责扫描素材、调用硬件加速转码 (H.265 Proxy)、提取元数据 (ffprobe) 并可靠上传至服务端。

## 核心特性

*   **🚀 高性能转码**: 利用 macOS `hevc_videotoolbox` 进行硬件加速，生成轻量级 H.265 代理文件。
*   **📡 断点续传**: 支持大文件断点续传，网络中断后自动从断点恢复，无需重传。
*   **🔍 智能变更检测**: 自动检测服务端文件状态，防止重复上传，支持文件变更后的自动更新。
*   **🛡️ 数据完整性**: 上传过程包含 MD5 校验 (X-File-Hash)，确保数据传输无误。
*   **ℹ️ 元数据集成**: 自动调用 `ffprobe` 提取视频元数据（时长、分辨率、帧率等）并同步至数据库。
*   **☕️ 防休眠 (macOS)**: 客户端运行时自动调用 `caffeinate` 防止系统休眠，保障任务不中断。

---

## 快速开始

### 0. 编译

具体内容请参考 [Usage.md](Usage.md)。

### 1. 服务端 (Server)

位于 `server` 目录。

**依赖**:
*   Go 1.20+
*   GCC (用于 SQLite CGO)

**运行**:
```bash
./mam-server -env my_config.env
```

服务端默认监听 `:8080` 端口。数据存储在本地 `mam.db` 和 `storage/` 目录。

### 2. 客户端 (Client)

位于 `client` 目录。需要安装 `ffmpeg`。

**依赖**:
*   Go 1.20+
*   FFmpeg (需支持 `hevc_videotoolbox`，如果是 macOS 通常默认支持):
    ```bash
    brew install ffmpeg
    ```

**编译**:
```bash
cd client
go mod tidy
go build -o ingest_client .
```

**使用**:
```bash
# 基本用法
./ingest_client -project "MyMovie" -source "/Volumes/CardA" -server "http://192.168.1.100:8080"

# 参数说明
# -project : 项目名称 (必填)
# -source  : 素材源目录 (默认为当前目录)
# -server  : 服务端地址 (默认为 http://localhost:8080)
```

## API 接口说明

核心接口采用 RESTful 风格与 Raw Binary 流式上传。

*   **检查状态**: `GET /api/dailies/check`
*   **断点上传**: `POST /api/dailies/upload`
    *   Header: `X-Upload-Offset`, `X-File-Hash`, `X-File-Metadata`
    *   Body: Binary Stream

## 开发指南

*   **服务端配置**: 在 `server/main.go` 头部常量可修改端口、管理员密码及 WebDAV 配置。
*   **并发控制**: 客户端默认开启 3 个转码 Worker 和 5 个上传 Worker，可在 `client/ingest_client.go` 中调整。

## 下一步计划

* 针对 Windows 设备优化客户端