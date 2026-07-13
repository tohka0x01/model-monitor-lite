#!/usr/bin/env bash
set -euo pipefail

#######################################
# NewAPI Model Monitor Lite - Linux 一键安装脚本
#
# 用法:
#   bash install-linux.sh
#   bash install-linux.sh --status
#   bash install-linux.sh --logs
#   bash install-linux.sh --update
#   bash install-linux.sh --uninstall
#
# 说明:
#   只部署 model-monitor-lite 自身，不修改 NewAPI 容器、数据库或表结构。
#######################################

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[OK]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
die() { log_error "$*"; exit 1; }

SERVICE_NAME="model-monitor-lite"
REPO_URL="${REPO_URL:-https://github.com/tohka0x01/model-monitor-lite.git}"
IMAGE="${IMAGE:-ghcr.io/tohka0x01/model-monitor-lite:latest}"
BUILD_LOCAL="${BUILD_LOCAL:-false}"
INSTALL_DIR="${INSTALL_DIR:-/opt/newapi-model-monitor-lite}"
SERVER_PORT="${SERVER_PORT:-1145}"
PUBLIC_TITLE="${PUBLIC_TITLE:-模型状态监控}"
DEFAULT_WINDOW="${DEFAULT_WINDOW:-24h}"
REFRESH_SECONDS="${REFRESH_SECONDS:-60}"
MAX_MODELS="${MAX_MODELS:-100}"
STATUS_TIMEOUT_SECONDS="${STATUS_TIMEOUT_SECONDS:-15}"
HISTORY_REFRESH_SECONDS="${HISTORY_REFRESH_SECONDS:-60}"
HISTORY_TIMEOUT_SECONDS="${HISTORY_TIMEOUT_SECONDS:-300}"
BASE_PATH="${BASE_PATH:-}"
MOCK_DATA="${MOCK_DATA:-false}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || echo "")"
WORK_DIR=""
DOCKER_COMPOSE=""

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "缺少必要命令: $1"
}

detect_docker_compose() {
  if docker compose version >/dev/null 2>&1; then
    DOCKER_COMPOSE="docker compose"
  elif command -v docker-compose >/dev/null 2>&1; then
    DOCKER_COMPOSE="docker-compose"
  else
    die "缺少 docker compose 或 docker-compose"
  fi
}

trim() {
  sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}

confirm() {
  local prompt="$1"
  local default_yes="${2:-true}"
  local answer
  if [[ "$default_yes" == "true" ]]; then
    read -r -p "$prompt [Y/n]: " answer
    [[ ! "$answer" =~ ^[nN]$ ]]
  else
    read -r -p "$prompt [y/N]: " answer
    [[ "$answer" =~ ^[yY]$ ]]
  fi
}

detect_newapi_container() {
  if [[ -n "${NEWAPI_CONTAINER:-}" ]]; then
    echo "$NEWAPI_CONTAINER"
    return 0
  fi

  local found=""
  found="$(docker ps --format '{{.Names}}' | awk 'tolower($0) ~ /(^|[-_])new-api([-_]|$)/ {print; exit}')"
  [[ -n "$found" ]] && { echo "$found"; return 0; }

  found="$(docker ps --filter 'label=com.docker.compose.service=new-api' --format '{{.Names}}' | head -n 1)"
  [[ -n "$found" ]] && { echo "$found"; return 0; }

  found="$(docker ps --format '{{.Names}}\t{{.Image}}' | awk -F'\t' 'tolower($2) ~ /(^|\/)new-api([-_:]|$)/ {print $1; exit}')"
  [[ -n "$found" ]] && { echo "$found"; return 0; }

  return 1
}

docker_env_value() {
  local container="$1"
  local key="$2"
  docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$container" 2>/dev/null \
    | awk -F= -v k="$key" '$1==k{print substr($0, length(k)+2); exit}'
}

first_existing() {
  local value
  for value in "$@"; do
    if [[ -n "$value" ]]; then
      echo "$value"
      return 0
    fi
  done
  return 0
}

detect_dsn_from_newapi() {
  SQL_DSN="${SQL_DSN:-}"
  LOG_SQL_DSN="${LOG_SQL_DSN:-}"

  local container=""
  container="$(detect_newapi_container || true)"
  if [[ -z "$container" ]]; then
    log_warn "未找到运行中的 NewAPI 容器"
    return 0
  fi

  log_success "检测到 NewAPI 容器: $container"
  SQL_DSN="$(first_existing "$SQL_DSN" "$(docker_env_value "$container" SQL_DSN)" "$(docker_env_value "$container" DATABASE_URL)" "$(docker_env_value "$container" DB_DSN)")"
  LOG_SQL_DSN="$(first_existing "$LOG_SQL_DSN" "$(docker_env_value "$container" LOG_SQL_DSN)")"

  if [[ -n "$SQL_DSN" ]]; then
    log_success "已读取 NewAPI SQL_DSN"
  fi
  if [[ -n "$LOG_SQL_DSN" ]]; then
    log_success "已读取 NewAPI LOG_SQL_DSN，监控将优先读取日志库"
  fi
}

resolve_source_dir() {
  if [[ -n "$SCRIPT_DIR" && -f "${SCRIPT_DIR}/go.mod" && -f "${SCRIPT_DIR}/main.go" ]]; then
    echo "$SCRIPT_DIR"
    return 0
  fi
  return 1
}

prepare_work_dir() {
  if [[ "$(id -u)" -ne 0 && "$INSTALL_DIR" == /opt/* ]]; then
    log_warn "当前不是 root，默认安装目录 /opt 可能无权限"
    INSTALL_DIR="${HOME}/newapi-model-monitor-lite"
    log_info "安装目录切换为: $INSTALL_DIR"
  fi

  mkdir -p "$INSTALL_DIR"
  WORK_DIR="$INSTALL_DIR"
}

sync_project_files() {
  if [[ "$BUILD_LOCAL" != "true" ]]; then
    log_info "使用预构建镜像，跳过源码同步"
    return 0
  fi

  local source_dir=""
  source_dir="$(resolve_source_dir || true)"

  if [[ -n "$source_dir" ]]; then
    if [[ "$(cd "$source_dir" && pwd)" == "$(cd "$WORK_DIR" && pwd)" ]]; then
      log_info "当前已在 model-monitor-lite 源码目录，跳过文件复制"
      return 0
    fi
    log_info "从本地源码复制 model-monitor-lite 文件..."
    mkdir -p "$WORK_DIR/static"
    cp "$source_dir/go.mod" "$WORK_DIR/go.mod"
    cp "$source_dir/go.sum" "$WORK_DIR/go.sum"
    cp "$source_dir"/*.go "$WORK_DIR/"
    cp "$source_dir/Dockerfile" "$WORK_DIR/Dockerfile"
    cp "$source_dir/static/embed.html" "$WORK_DIR/static/embed.html"
    cp "$source_dir/static/embed.js" "$WORK_DIR/static/embed.js"
    cp "$source_dir/static/style.css" "$WORK_DIR/static/style.css"
    cp "$source_dir/docker-compose.example.yml" "$WORK_DIR/docker-compose.example.yml" 2>/dev/null || true
    return 0
  fi

  need_cmd git
  local repo_dir="${WORK_DIR}/repo"
  if [[ -d "${repo_dir}/.git" ]]; then
    log_info "更新仓库源码..."
    git -C "$repo_dir" fetch --depth=1 origin main
    git -C "$repo_dir" reset --hard origin/main
  else
    log_info "克隆仓库源码..."
    rm -rf "$repo_dir"
    git clone --depth=1 "$REPO_URL" "$repo_dir"
  fi

  if [[ -d "${repo_dir}/model-monitor-lite" ]]; then
    repo_dir="${repo_dir}/model-monitor-lite"
  fi
  [[ -f "${repo_dir}/go.mod" && -f "${repo_dir}/main.go" ]] || die "仓库中未找到 model-monitor-lite 源码"
  mkdir -p "$WORK_DIR/static"
  cp "${repo_dir}/go.mod" "$WORK_DIR/go.mod"
  cp "${repo_dir}/go.sum" "$WORK_DIR/go.sum"
  cp "${repo_dir}"/*.go "$WORK_DIR/"
  cp "${repo_dir}/Dockerfile" "$WORK_DIR/Dockerfile"
  cp "${repo_dir}/static/embed.html" "$WORK_DIR/static/embed.html"
  cp "${repo_dir}/static/embed.js" "$WORK_DIR/static/embed.js"
  cp "${repo_dir}/static/style.css" "$WORK_DIR/static/style.css"
}

write_env_file() {
  local env_file="${WORK_DIR}/.env"
  if [[ -z "$SQL_DSN" && "$MOCK_DATA" != "true" ]]; then
    echo ""
    log_warn "未检测到 SQL_DSN，且未启用 MOCK_DATA"
    read -r -p "请输入 NewAPI SQL_DSN（留空则启用 MOCK_DATA=true）: " SQL_DSN
    if [[ -z "$SQL_DSN" ]]; then
      MOCK_DATA="true"
      log_warn "已启用 MOCK_DATA=true，仅用于预览 UI，不会读取真实 NewAPI 日志"
    fi
  fi

  if [[ -f "$env_file" ]]; then
    cp "$env_file" "${env_file}.backup.$(date +%Y%m%d_%H%M%S)"
    log_info "已备份旧配置文件"
  fi

  cat > "$env_file" <<EOF
# NewAPI Model Monitor Lite 配置
# 由 install-linux.sh 生成于 $(date '+%Y-%m-%d %H:%M:%S')

SQL_DSN=${SQL_DSN}
LOG_SQL_DSN=${LOG_SQL_DSN}
SERVER_HOST=0.0.0.0
SERVER_PORT=${SERVER_PORT}
BASE_PATH=${BASE_PATH}
PUBLIC_TITLE=${PUBLIC_TITLE}
DEFAULT_MODELS=${DEFAULT_MODELS:-}
DEFAULT_WINDOW=${DEFAULT_WINDOW}
REFRESH_SECONDS=${REFRESH_SECONDS}
MAX_MODELS=${MAX_MODELS}
STATUS_TIMEOUT_SECONDS=${STATUS_TIMEOUT_SECONDS}
HISTORY_DATA_PATH=/data/model-monitor.db
HISTORY_REFRESH_SECONDS=${HISTORY_REFRESH_SECONDS}
HISTORY_TIMEOUT_SECONDS=${HISTORY_TIMEOUT_SECONDS}
MOCK_DATA=${MOCK_DATA}
EOF

  chmod 600 "$env_file"
  log_success "配置文件已生成: $env_file"
}

write_compose_file() {
  local compose_file="${WORK_DIR}/docker-compose.yml"
  if [[ "$BUILD_LOCAL" == "true" ]]; then
    cat > "$compose_file" <<EOF
services:
  ${SERVICE_NAME}:
    build: .
    container_name: ${SERVICE_NAME}
    env_file:
      - .env
    ports:
      - "127.0.0.1:${SERVER_PORT}:${SERVER_PORT}"
    restart: unless-stopped
EOF
  else
    cat > "$compose_file" <<EOF
services:
  ${SERVICE_NAME}:
    image: ${IMAGE}
    container_name: ${SERVICE_NAME}
    env_file:
      - .env
    ports:
      - "127.0.0.1:${SERVER_PORT}:${SERVER_PORT}"
    restart: unless-stopped
EOF
  fi
  log_success "Docker Compose 文件已生成: $compose_file"
}

ensure_history_storage() {
  mkdir -p "${WORK_DIR}/data"
  cat > "${WORK_DIR}/docker-compose.override.yml" <<EOF
services:
  ${SERVICE_NAME}:
    environment:
      HISTORY_DATA_PATH: /data/model-monitor.db
    volumes:
      - ./data:/data
EOF
  log_success "历史统计目录已就绪: ${WORK_DIR}/data"
}

install_management_script() {
  local source_script="${BASH_SOURCE[0]}"
  local target_script="${WORK_DIR}/install-linux.sh"
  [[ -f "$source_script" ]] || die "未找到安装脚本: $source_script"
  if [[ "$(cd "$(dirname "$source_script")" && pwd)/$(basename "$source_script")" != "$target_script" ]]; then
    cp "$source_script" "$target_script"
  fi
  chmod 755 "$target_script"
}

connect_to_newapi_network() {
  local container=""
  container="$(detect_newapi_container || true)"
  [[ -n "$container" ]] || return 0

  local networks=""
  networks="$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{println $k}}{{end}}' "$container" 2>/dev/null || true)"
  local network=""
  network="$(echo "$networks" | awk '$0!="bridge" && $0!="host" && $0!="none"{print; exit}')"
  [[ -n "$network" ]] || return 0

  log_info "尝试把 ${SERVICE_NAME} 接入 NewAPI 网络: $network"
  docker network connect "$network" "$SERVICE_NAME" 2>/dev/null || log_info "网络已连接或无需重复连接"
}

start_service() {
  log_info "启动 ${SERVICE_NAME}..."
  if [[ "$BUILD_LOCAL" == "true" ]]; then
    (cd "$WORK_DIR" && $DOCKER_COMPOSE --env-file .env up -d --build)
  else
    (cd "$WORK_DIR" && $DOCKER_COMPOSE --env-file .env pull && $DOCKER_COMPOSE --env-file .env up -d)
  fi
  connect_to_newapi_network
  log_success "服务已启动"
}

wait_for_health() {
  local env_file="${WORK_DIR}/.env"
  local port=""
  local base_path=""
  port="$(awk -F= '$1=="SERVER_PORT"{print substr($0, length($1)+2); exit}' "$env_file")"
  base_path="$(awk -F= '$1=="BASE_PATH"{print substr($0, length($1)+2); exit}' "$env_file")"
  [[ -n "$port" ]] || die "配置文件缺少 SERVER_PORT"

  local health_url="http://127.0.0.1:${port}${base_path}/api/health"
  for _ in $(seq 1 30); do
    if curl -fsS --max-time 2 "$health_url" >/dev/null; then
      log_success "健康检查通过: $health_url"
      return 0
    fi
    sleep 1
  done
  (cd "$WORK_DIR" && $DOCKER_COMPOSE logs --tail=100) || true
  die "更新后健康检查失败: $health_url"
}

update_service() {
  need_cmd docker
  need_cmd curl
  prepare_work_dir
  detect_docker_compose
  [[ -f "${WORK_DIR}/docker-compose.yml" ]] || die "未找到 ${WORK_DIR}/docker-compose.yml，请先安装"
  [[ -f "${WORK_DIR}/.env" ]] || die "未找到 ${WORK_DIR}/.env，请先安装"
  grep -qE '^[[:space:]]+image:' "${WORK_DIR}/docker-compose.yml" \
    || die "--update 仅支持预构建镜像部署；本地构建请重新运行 BUILD_LOCAL=true install-linux.sh"

  ensure_history_storage
  install_management_script
  log_info "拉取最新镜像并重建 ${SERVICE_NAME}..."
  (cd "$WORK_DIR" && $DOCKER_COMPOSE --env-file .env pull)
  (cd "$WORK_DIR" && $DOCKER_COMPOSE --env-file .env up -d --force-recreate)
  connect_to_newapi_network
  wait_for_health
  log_success "${SERVICE_NAME} 已更新到最新镜像"
}

show_result() {
  echo ""
  echo -e "${GREEN}========================================${NC}"
  echo -e "${GREEN}  NewAPI Model Monitor Lite 安装完成${NC}"
  echo -e "${GREEN}========================================${NC}"
  echo ""
  echo -e "安装目录: ${YELLOW}${WORK_DIR}${NC}"
  echo -e "容器名称: ${YELLOW}${SERVICE_NAME}${NC}"
  echo -e "本机地址: ${BLUE}http://127.0.0.1:${SERVER_PORT}${BASE_PATH}/embed${NC}"
  echo -e "反代提示: ${YELLOW}请通过 NewAPI / Nginx / Cloudflare Tunnel 暴露到你的域名后再嵌入${NC}"
  echo ""
  echo "NewAPI iframe 示例:"
  echo "  <iframe src=\"/model-monitor/embed\" style=\"width:100%;height:720px;border:0;\" loading=\"lazy\"></iframe>"
  echo ""
  echo "常用命令:"
  echo "  cd ${WORK_DIR} && ${DOCKER_COMPOSE} ps"
  echo "  cd ${WORK_DIR} && ${DOCKER_COMPOSE} logs -f --tail=100"
  echo "  cd ${WORK_DIR} && ${DOCKER_COMPOSE} restart"
  echo "  bash ${WORK_DIR}/install-linux.sh --update"
  echo ""
}

show_status() {
  prepare_work_dir
  detect_docker_compose
  if [[ ! -f "${WORK_DIR}/docker-compose.yml" ]]; then
    die "未找到 ${WORK_DIR}/docker-compose.yml"
  fi
  (cd "$WORK_DIR" && $DOCKER_COMPOSE ps)
}

show_logs() {
  prepare_work_dir
  detect_docker_compose
  if [[ ! -f "${WORK_DIR}/docker-compose.yml" ]]; then
    die "未找到 ${WORK_DIR}/docker-compose.yml"
  fi
  (cd "$WORK_DIR" && $DOCKER_COMPOSE logs -f --tail=100)
}

uninstall_service() {
  prepare_work_dir
  detect_docker_compose
  if [[ -f "${WORK_DIR}/docker-compose.yml" ]]; then
    (cd "$WORK_DIR" && $DOCKER_COMPOSE down --remove-orphans)
  else
    docker rm -f "$SERVICE_NAME" 2>/dev/null || true
  fi

  if confirm "是否删除安装目录 ${WORK_DIR} ?" false; then
    [[ -n "$WORK_DIR" && "$WORK_DIR" != "/" && "$WORK_DIR" != "$HOME" ]] || die "拒绝删除危险目录: ${WORK_DIR}"
    rm -rf "$WORK_DIR"
    log_success "安装目录已删除"
  else
    log_info "保留安装目录: $WORK_DIR"
  fi
}

install_service() {
  need_cmd docker
  detect_docker_compose

  echo ""
  echo -e "${BLUE}========================================${NC}"
  echo -e "${BLUE}  NewAPI Model Monitor Lite Linux 安装${NC}"
  echo -e "${BLUE}========================================${NC}"
  echo ""

  detect_dsn_from_newapi
  prepare_work_dir

  echo -e "安装目录: ${YELLOW}${WORK_DIR}${NC}"
  echo -e "服务端口: ${YELLOW}${SERVER_PORT}${NC}"
  echo -e "标题:     ${YELLOW}${PUBLIC_TITLE}${NC}"
  echo -e "Mock:     ${YELLOW}${MOCK_DATA}${NC}"
  echo -e "镜像:     ${YELLOW}${IMAGE}${NC}"
  echo -e "本地构建: ${YELLOW}${BUILD_LOCAL}${NC}"
  echo ""
  if ! confirm "继续安装/更新 ${SERVICE_NAME} ?" true; then
    log_info "已取消"
    exit 0
  fi

  sync_project_files
  write_env_file
  write_compose_file
  ensure_history_storage
  install_management_script
  start_service
  show_result
}

show_help() {
  cat <<EOF
NewAPI Model Monitor Lite - Linux 一键安装脚本

用法:
  install-linux.sh              交互式安装或更新
  install-linux.sh --status     查看容器状态
  install-linux.sh --logs       查看实时日志
  install-linux.sh --update     拉取最新镜像并保留配置与历史数据
  install-linux.sh --uninstall  停止并卸载本模块
  install-linux.sh --help       显示帮助

常用环境变量:
  INSTALL_DIR=/opt/newapi-model-monitor-lite
  SERVER_PORT=1145
  IMAGE='ghcr.io/tohka0x01/model-monitor-lite:latest'
  BUILD_LOCAL=false
  SQL_DSN='host=postgres port=5432 user=postgres password=xxx dbname=new-api sslmode=disable'
  LOG_SQL_DSN=''
  BASE_PATH=''
  PUBLIC_TITLE='模型状态监控'
  DEFAULT_MODELS='gpt-4o,claude-3-5-sonnet'
  REFRESH_SECONDS=60
  MAX_MODELS=100
  STATUS_TIMEOUT_SECONDS=15
  HISTORY_REFRESH_SECONDS=60
  HISTORY_TIMEOUT_SECONDS=300
  MOCK_DATA=false
  NEWAPI_CONTAINER=new-api
EOF
}

main() {
  local action="${1:-}"
  case "$action" in
    "" )
      install_service
      ;;
    --status )
      show_status
      ;;
    --logs )
      show_logs
      ;;
    --update )
      update_service
      ;;
    --uninstall )
      uninstall_service
      ;;
    --help|-h )
      show_help
      ;;
    * )
      die "未知参数: $action，使用 --help 查看帮助"
      ;;
  esac
}

main "$@"
