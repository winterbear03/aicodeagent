# AI 零代码生成平台

[![Go Version](https://img.shields.io/badge/Go-1.26-blue.svg)](https://go.dev/)
[![Vue Version](https://img.shields.io/badge/Vue-3.x-green.svg)](https://vuejs.org/)
[![Docker](https://img.shields.io/badge/Docker-✔-blue.svg)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> 基于 Go + Vue 3 的全栈 AI 智能体平台，支持多轮对话上下文、代码生成、对象存储、实时推送与容器化一键部署。

核心亮点：输入自然语言需求，AI 自动生成可运行的代码，并支持追问修改、历史会话管理、代码包云存储下载。

## 功能特性

- AI 智能体调度：多工具动态注册（代码生成、知识库检索、HTML 截图、代码打包），Agent 智能路由。
- 多轮对话上下文：基于会话 ID 自动携带历史对话，支持追问、修改、补充。
- 异步任务队列：Worker Pool (5 并发) + Channel 实现高并发任务处理。
- 实时推送：WebSocket 主动推送任务结果，无需前端轮询。
- 三级缓存架构：本地内存 → Redis → MySQL，热点任务响应 < 10ms。
- 对象存储集成：MinIO 存储生成代码与截图，预签名 URL 安全下载。
- 容器化部署：Docker Compose 一键编排 MySQL、Redis、MinIO、Nginx 及后端服务。
- 监控体系：Prometheus 指标暴露，支持 Grafana 可视化。
- 内网穿透：Serveo SSH 反向隧道实现公网访问（可选）。

技术栈

| 层级 | 技术 |
|------|------|
| 后端 | Go 1.26, Gin, GORM, Gorilla WebSocket |
| 前端 | Vue 3, Naive UI / 自定义 Gemini 风格 |
| 数据库 | MySQL 8.0 (持久化), Redis 7 (缓存) |
| 存储 | MinIO (对象存储) |
| AI 模型 | 智谱 AI (GLM-4-Flash) |
| 部署 | Docker, Docker Compose, Nginx |
| 监控 | Prometheus, Grafana (可选) |

📐 系统架构
┌─────────────┐ ┌─────────────┐ ┌─────────────┐
│ Browser │────▶│ Nginx:80 │────▶│ Gin :8080 │
│ (Vue 3) │◀────│ (Proxy) │◀────│ (API) │
└─────────────┘ └─────────────┘ └──────┬──────┘
│
┌─────────────────┼─────────────────┐
▼ ▼ ▼
┌─────────┐ ┌─────────┐ ┌─────────┐
│ MySQL │ │ Redis │ │ MinIO │
│ (持久化)│ │ (缓存) │ │ (对象) │
└─────────┘ └─────────┘ └─────────┘


🚀 快速开始

前置要求

- [Docker](https://www.docker.com/) 20.10+
- [Docker Compose](https://docs.docker.com/compose/) v2+
- 智谱 AI API Key（[免费申请](https://open.bigmodel.cn/)）

配置环境变量
export ZHIPU_API_KEY="你的智谱API密钥"

一键启动
docker-compose up -d --build