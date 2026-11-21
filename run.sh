#!/usr/bin/env bash
# DNS 区数据采集站 —— 一站式启动入口。
#
# 用法：
#   ./run.sh            构建并启动（网页 :8080，测试 DNS 127.0.0.1:5353）
#   ./run.sh -http :9090 -dns 127.0.0.1:15353
#
# 单个进程内同时运行：本地权威测试 DNS 服务、采集引擎、网页界面。
set -euo pipefail
cd "$(dirname "$0")"

# 定位 Go 工具链：PATH → 项目内 .tools → 常见安装位置。
find_go() {
  if command -v go >/dev/null 2>&1; then
    command -v go
    return 0
  fi
  for cand in ./.tools/go/bin/go /usr/local/go/bin/go /usr/lib/go/bin/go "$HOME/go-sdk/go/bin/go" "$HOME/sdk/go/bin/go"; do
    if [ -x "$cand" ]; then
      echo "$cand"
      return 0
    fi
  done
  return 1
}

GO="$(find_go)" || {
  echo "未找到 Go 工具链。请安装 Go 1.22+，或把工具链放到 ./.tools/go。" >&2
  exit 1
}

# 首次构建需要拉取依赖；网络受限时可通过 GOPROXY 环境变量覆盖。
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOFLAGS="${GOFLAGS:-} -mod=mod"

echo "==> 使用 Go: $("$GO" version)"
echo "==> 构建 zonestation …"
mkdir -p ./.tools
"$GO" build -o ./.tools/zonestation ./cmd/zonestation

echo "==> 启动（Ctrl-C 停止）"
exec ./.tools/zonestation "$@"
