# collect-ai S 端 (后端) Linux 服务器部署

完整 c/s 架构: Win7 C 端 + Linux S 端 (后端) + 飞书 H5 (可选)

## 架构

```
┌─────────── Win7 SP1 (本机) ───────────┐         ┌─────────── Linux 服务器 ───────────┐
│  collect-ai.exe (C 端)               │  HTTP   │  docker compose:                    │
│  - 选供应商 / 上传图 / 编辑 / 导出   │ ──────> │    pg            (PostgreSQL 16)    │
│  - 全部调后端 (零 OCR/LLM)            │  JSON   │    server        (Go 后端)         │
│                                       │ <────── │    cube-agent   (SKU 库)           │
└─────────────────────────────────────┘         └───────────────────────────────────────┘
                                              + 可选: feishuapp (F:\feishuapp\collect-ai)
```

## 一键部署 (docker compose)

### 1. 准备 Linux 服务器
- Ubuntu 22.04+ / CentOS 8+ / Debian 11+
- 安装 Docker + Docker Compose
- 开放 8089 端口 (Win7 客户端访问)

### 2. 传文件到 Linux

将本目录 (含 `Dockerfile` + `docker-compose.yml`) 复制到 Linux, 例如:
```bash
scp -r F:\go\src\github.com\tinkler\collect-ai your-user@server:/opt/
```

### 3. 填 BigModel Key

```bash
cd /opt/collect-ai
cp .env.example .env
nano .env  # 填 BIGMODEL_API_KEY=xxx
```

### 4. 一键启动

```bash
docker compose up -d
# 启动 3 个服务:
#   collectai-pg         (PostgreSQL 16, 端口 5432 内部)
#   collectai-server     (Go 后端,  端口 8089 对外)
#   collectai-cube-agent (SKU 库, 端口 8088 对外/可选)
```

### 5. 验证

```bash
# 健康检查
curl http://localhost:8089/api/v1/health
# {"status":"ok","ts":...}

# 列出 52 个供应商 (需 cube-agent 数据)
curl http://localhost:8089/api/v1/suppliers | head -c 200

# 看 log
docker compose logs -f server
```

### 6. 配置 Win7 C 端

Win7 启动 collect-ai.exe, 顶部"后端 URL"填:
```
http://<server-ip>:8089
```

例如服务器 IP 是 192.168.1.100:
```
http://192.168.1.100:8089
```

保存后所有按钮激活 (识别/保存/历史/重新匹配/同步模板).

### 7. 防火墙

Linux 服务器开放 8089:
```bash
# Ubuntu
sudo ufw allow 8089/tcp
sudo ufw reload

# CentOS
sudo firewall-cmd --add-port=8089/tcp --permanent
sudo firewall-cmd --reload
```

Win7 出站 (Win7 默认允许, 不用配).

## 飞书 H5 (可选)

如果启用飞书 H5, 还需要:
1. `cd /opt/feishuapp` (飞书 H5 文件)
2. 用 nginx 静态服务, 反代 /api → http://localhost:8089
3. 飞书后台配置 H5 应用 + 应用 URL

详见 `F:\feishuapp\collect-ai\README.md`


## 跨平台编译 (无 Docker)

如果不想用 Docker, 直接跑 Go 二进制:

```bash
# 在本地 build
cd F:\go\src\github.com\tinkler\collect-ai
set GOOS=linux
set GOARCH=amd64
go build -ldflags="-s -w" -o collect-ai-server-linux-amd64 ./cmd/server

# 传到 Linux
scp collect-ai-server-linux-amd64 your-user@server:/opt/collect-ai/

# 在 Linux 启 PG
sudo apt install postgresql-16
sudo -u postgres createdb collectai
# 跑迁移 (用 psql)
psql -U postgres -d collectai -f migrations/001_init.sql

# 启服务
cd /opt/collect-ai
./collect-ai-server-linux-amd64
```

## 升级

```bash
cd /opt/collect-ai
git pull   # 或 scp 覆盖
docker compose up -d --build
```

## 数据备份

```bash
# PG 数据 (解析历史)
docker exec collectai-pg pg_dump -U postgres collectai > backup.sql

# 上传图片
docker run --rm -v collectai-uploads:/data -v $(pwd):/backup alpine tar czf /backup/uploads.tar.gz /data
```

## 监控

```bash
# 限流状态
curl http://server:8089/api/v1/ratelimit/stats
# {"active":0,"max":4,"total_block":0,"total_wait":0}

# 服务状态
docker compose ps
```
