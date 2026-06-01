#!/usr/bin/env bash
#
# mail_go 服务安装/管理脚本
# 用法:
#   sudo ./install.sh install    — 安装/更新服务
#   sudo ./install.sh uninstall  — 卸载服务
#   sudo ./install.sh start      — 启动服务
#   sudo ./install.sh stop       — 停止服务
#   sudo ./install.sh restart    — 重启服务
#   sudo ./install.sh status     — 查看服务状态
#

source /etc/profile
source ~/.bashrc

set -euo pipefail

# ======================== 配置区 ========================
SERVICE_NAME="mail_go"
SERVICE_USER="mail_go"
SERVICE_DESC="MailGo - Go 邮件系统（SMTP/IMAP/POP3 + Web）"

# 目录
INSTALL_DIR="/opt/mail_go"            # 程序安装目录
DATA_DIR="/srv/mail_go"               # 数据目录（数据库+附件）
CONFIG_DIR="/etc/mail_go"             # 配置文件目录
LOG_DIR="/var/log/mail_go"            # 日志目录
PID_DIR="/run/mail_go"                # PID/Socket 目录

# Git 仓库
GIT_REPO="https://git.lmve.net/kevin/mailgo.git"
GIT_BRANCH="main"

# 编译临时目录
BUILD_DIR="/tmp/mail_go_build"

# systemd 服务文件路径
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

# ======================== 颜色输出 ========================
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

info()  { echo -e "${BLUE}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# ======================== 前置检查 ========================
check_root() {
    if [[ $EUID -ne 0 ]]; then
        error "此脚本需要 root 权限运行，请使用 sudo"
    fi
}

# ======================== 用户管理 ========================
ensure_user() {
    if id "${SERVICE_USER}" &>/dev/null; then
        ok "用户 ${SERVICE_USER} 已存在"
    else
        info "创建系统用户 ${SERVICE_USER} ..."
        useradd -r -s /usr/sbin/nologin -d "${INSTALL_DIR}" -c "${SERVICE_DESC}" "${SERVICE_USER}"
        ok "用户 ${SERVICE_USER} 创建成功"
    fi
}

# ======================== 目录管理 ========================
ensure_dirs() {
    info "检查并创建关键目录 ..."

    # 安装目录（含模板子目录结构）
    mkdir -p "${INSTALL_DIR}/internal/web/templates/admin"
    # 配置目录
    mkdir -p "${CONFIG_DIR}"
    # 数据目录及子目录
    mkdir -p "${DATA_DIR}/attachments"
    # 日志目录
    mkdir -p "${LOG_DIR}"
    # PID/Socket 目录
    mkdir -p "${PID_DIR}"

    # 设置所有权（每次安装都强制修正，避免旧版遗留权限问题）
    chown -R "${SERVICE_USER}:${SERVICE_USER}" "${INSTALL_DIR}"
    chown -R "${SERVICE_USER}:${SERVICE_USER}" "${DATA_DIR}"
    chown -R "${SERVICE_USER}:${SERVICE_USER}" "${LOG_DIR}"
    chown -R "${SERVICE_USER}:${SERVICE_USER}" "${PID_DIR}"
    chown -R "${SERVICE_USER}:${SERVICE_USER}" "${CONFIG_DIR}"

    # 确保目录可写
    chmod 755 "${CONFIG_DIR}" "${DATA_DIR}" "${LOG_DIR}" "${PID_DIR}" "${INSTALL_DIR}"

    ok "目录结构就绪"
}

# ======================== 编译 ========================
build_binary() {
    info "拉取最新代码 ..."
    if [[ -d "${BUILD_DIR}/.git" ]]; then
        cd "${BUILD_DIR}"
        git fetch --all
        git reset --hard "origin/${GIT_BRANCH}"
        info "代码已更新到最新"
    else
        rm -rf "${BUILD_DIR}"
        git clone -b "${GIT_BRANCH}" "${GIT_REPO}" "${BUILD_DIR}"
        cd "${BUILD_DIR}"
        info "代码克隆完成"
    fi

    info "编译 Go 项目（CGO 已启用，SQLite 依赖） ..."
    CGO_ENABLED=1 go build -ldflags="-s -w" -o mail_go .
    ok "编译完成: ${BUILD_DIR}/mail_go"
}

# ======================== 部署文件 ========================
deploy_files() {
    info "部署文件到 ${INSTALL_DIR} ..."

    # 停止服务（如果正在运行），避免替换正在使用的二进制
    if systemctl is-active --quiet "${SERVICE_NAME}" 2>/dev/null; then
        info "停止运行中的服务 ..."
        systemctl stop "${SERVICE_NAME}"
    fi

    # 复制二进制
    cp -f "${BUILD_DIR}/mail_go" "${INSTALL_DIR}/mail_go"
    chmod 755 "${INSTALL_DIR}/mail_go"

    # 复制模板文件（目录结构: internal/web/templates/*.html + admin/*.html）
    cp -f "${BUILD_DIR}/internal/web/templates/"*.html "${INSTALL_DIR}/internal/web/templates/"
    cp -f "${BUILD_DIR}/internal/web/templates/admin/"*.html "${INSTALL_DIR}/internal/web/templates/admin/"

    # 配置文件由程序首次启动时自动生成 + 缺失项自动补全，脚本不干预
    if [[ -f "${CONFIG_DIR}/mail_go.toml" ]]; then
        info "保留现有配置文件: ${CONFIG_DIR}/mail_go.toml"
    else
        info "配置文件不存在，程序首次启动将自动生成"
    fi

    # 设置安装目录所有权
    chown -R "${SERVICE_USER}:${SERVICE_USER}" "${INSTALL_DIR}"

    ok "文件部署完成"
}

# ======================== systemd 服务 ========================
install_service() {
    info "创建 systemd 服务文件 ..."

    cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=${SERVICE_DESC}
After=network.target
Wants=network.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
WorkingDirectory=${INSTALL_DIR}
ExecStart=${INSTALL_DIR}/mail_go
ExecReload=/bin/kill -HUP \$MAINPID
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

# 日志输出到 journald
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

# 安全加固：专用低权用户运行，无需额外文件系统沙箱
# ProtectSystem=strict/full 会阻止写入 /etc 和 /opt 等目录，
# 导致配置文件和 unix socket 无法创建，得不偿失
ProtectHome=true
NoNewPrivileges=true

# 环境变量
Environment=GIN_MODE=release

# MailGo 需要绑定 25/465/143/993/110/995 等特权端口
# 取消限制，允许非 root 绑定低端口号（需内核支持）
# 或使用: sysctl net.ipv4.ip_unprivileged_port_start=0
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF

    chmod 644 "${SERVICE_FILE}"
    systemctl daemon-reload
    ok "systemd 服务文件已创建"
}

# ======================== 安装主流程 ========================
do_install() {
    info "========== 安装 ${SERVICE_NAME} =========="

    # 1. 检查依赖
    check_root
    command -v git &>/dev/null || error "需要 git，请先安装"
    command -v go &>/dev/null  || error "需要 go，请先安装"
    command -v gcc &>/dev/null || error "需要 gcc（CGO/SQLite 编译依赖），请先安装"

    # 2. 创建用户
    ensure_user

    # 3. 创建目录
    ensure_dirs

    # 4. 拉取代码并编译
    build_binary

    # 5. 部署文件
    deploy_files

    # 6. 安装服务
    install_service

    # 7. 启动并设置开机自启
    info "启动服务 ..."
    systemctl start "${SERVICE_NAME}"
    systemctl enable "${SERVICE_NAME}"

    sleep 1
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        ok "服务启动成功！"
    else
        error "服务启动失败，请检查日志: journalctl -u ${SERVICE_NAME} -n 50"
    fi

    echo ""
    info "========== 安装完成 =========="
    echo "  程序目录: ${INSTALL_DIR}"
    echo "  配置文件: ${CONFIG_DIR}/mail_go.toml"
    echo "  数据目录: ${DATA_DIR}"
    echo "  附件目录: ${DATA_DIR}/attachments"
    echo "  日志目录: ${LOG_DIR}"
    echo "  服务管理: systemctl {start|stop|restart|status} ${SERVICE_NAME}"
    echo "  查看日志: journalctl -u ${SERVICE_NAME} -f"
    echo ""
    echo "  默认监听端口:"
    echo "    SMTP  25 / 465(TLS)    IMAP  143 / 993(TLS)"
    echo "    POP3  110 / 995(TLS)   Web   :8080"
    echo ""
    echo "  默认管理员: admin@example.com / admin"
    warn "⚠ 请登录后立即修改默认密码！"
}

# ======================== 卸载 ========================
do_uninstall() {
    info "========== 卸载 ${SERVICE_NAME} =========="
    check_root

    # 停止服务
    if systemctl is-active --quiet "${SERVICE_NAME}" 2>/dev/null; then
        systemctl stop "${SERVICE_NAME}"
        info "服务已停止"
    fi

    # 禁用开机自启
    if systemctl is-enabled --quiet "${SERVICE_NAME}" 2>/dev/null; then
        systemctl disable "${SERVICE_NAME}"
        info "已禁用开机自启"
    fi

    # 删除服务文件
    if [[ -f "${SERVICE_FILE}" ]]; then
        rm -f "${SERVICE_FILE}"
        systemctl daemon-reload
        info "服务文件已删除"
    fi

    # 询问是否删除数据
    echo ""
    warn "以下目录包含运行数据，是否删除？"
    echo "  [1] ${INSTALL_DIR}  (程序文件+模板)"
    echo "  [2] ${DATA_DIR}     (数据库+附件)"
    echo "  [3] ${CONFIG_DIR}   (配置文件)"
    echo "  [4] ${LOG_DIR}      (日志)"
    echo "  [0] 全部保留（默认）"
    echo "  [a] 全部删除"
    echo ""
    read -rp "请选择 [0/a/组合如 14]: " choice

    case "${choice:-0}" in
        0)
            info "保留所有数据目录"
            ;;
        a|A)
            rm -rf "${INSTALL_DIR}" "${DATA_DIR}" "${CONFIG_DIR}" "${LOG_DIR}"
            info "所有数据目录已删除"
            ;;
        *)
            [[ "${choice}" == *1* ]] && rm -rf "${INSTALL_DIR}" && info "已删除 ${INSTALL_DIR}"
            [[ "${choice}" == *2* ]] && rm -rf "${DATA_DIR}" && info "已删除 ${DATA_DIR}"
            [[ "${choice}" == *3* ]] && rm -rf "${CONFIG_DIR}" && info "已删除 ${CONFIG_DIR}"
            [[ "${choice}" == *4* ]] && rm -rf "${LOG_DIR}" && info "已删除 ${LOG_DIR}"
            ;;
    esac

    # 删除用户（如果没有其他依赖）
    if id "${SERVICE_USER}" &>/dev/null; then
        read -rp "是否删除系统用户 ${SERVICE_USER}？[y/N]: " del_user
        if [[ "${del_user}" =~ ^[Yy]$ ]]; then
            userdel "${SERVICE_USER}"
            info "用户 ${SERVICE_USER} 已删除"
        fi
    fi

    # 清理编译目录
    rm -rf "${BUILD_DIR}"

    ok "卸载完成"
}

# ======================== 服务控制 ========================
do_start() {
    check_root
    systemctl start "${SERVICE_NAME}"
    sleep 1
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        ok "服务已启动"
    else
        error "服务启动失败，查看日志: journalctl -u ${SERVICE_NAME} -n 50"
    fi
}

do_stop() {
    check_root
    systemctl stop "${SERVICE_NAME}"
    ok "服务已停止"
}

do_restart() {
    check_root
    systemctl restart "${SERVICE_NAME}"
    sleep 1
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        ok "服务已重启"
    else
        error "服务重启失败，查看日志: journalctl -u ${SERVICE_NAME} -n 50"
    fi
}

do_status() {
    systemctl status "${SERVICE_NAME}" --no-pager || true
}

# ======================== 入口 ========================
case "${1:-}" in
    install)   do_install   ;;
    uninstall) do_uninstall ;;
    start)     do_start     ;;
    stop)      do_stop      ;;
    restart)   do_restart   ;;
    status)    do_status    ;;
    *)
        echo "用法: sudo $0 {install|uninstall|start|stop|restart|status}"
        echo ""
        echo "  install   — 完整安装/更新（拉代码+编译+部署+启动+开机自启）"
        echo "  uninstall — 卸载服务（可选保留数据）"
        echo "  start     — 启动服务"
        echo "  stop      — 停止服务"
        echo "  restart   — 重启服务"
        echo "  status    — 查看服务状态"
        exit 1
        ;;
esac
