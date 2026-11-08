#!/usr/bin/env bash
# yika.sh —— 驿卡一键部署（在开发机上执行；macOS / Linux，需要 go、node + corepack、ssh、scp）。
#
#   deploy/remote/yika.sh <目标> deploy            编译 → 打包 → 上传 → 在服务器上安装/升级并重启（可重复执行）
#   deploy/remote/yika.sh <目标> status            服务状态与健康检查
#   deploy/remote/yika.sh <目标> logs [服务] [行数] 查看日志（isp-control | gost-agent | nginx，默认 isp-control）
#   deploy/remote/yika.sh <目标> mail [N]          查看最近 N 封邮件（mail.mode = file 时验证码在这里）
#   deploy/remote/yika.sh <目标> backup [本机目录]  完整备份（数据库 + 主密钥 + 证书 + 节点 ID）下载到本机
#   deploy/remote/yika.sh <目标> restore <备份文件> 把备份恢复到一台全新服务器并完成部署（换服务器用）
#   deploy/remote/yika.sh <目标> ssh [命令]        登录服务器或执行一条命令
#
# <目标> 是 deploy/remote/targets/<目标>.env（不进 Git，示例见 target.example.env），也可直接给 .env 文件路径。
# 换服务器：新建一份目标配置 → 旧目标 backup → 新目标 restore <备份文件>。
#
# 可选环境变量：SKIP_BUILD=1（沿用上次的编译产物）、BUNDLE_DIR（打包目录，默认临时目录）。
set -euo pipefail
# 变量一律写成 ${var}：bash 3.2（macOS）在 UTF-8 下会把紧跟的全角标点的首字节当作变量名的一部分。

root="$(cd "$(dirname "$0")/../.." && pwd)"
remote_dir=/root/yika-bundle
pnpm="corepack pnpm@10.28.1"

die() { printf '错误：%s\n' "$*" >&2; exit 1; }
step() { printf '\n==> %s\n' "$*"; }

[ $# -ge 1 ] || { sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
target="$1"
cmd="${2:-deploy}"
shift 2 2>/dev/null || shift $#

case "${target}" in
  */* | *.env) envfile="${target}" ;;
  *) envfile="${root}/deploy/remote/targets/${target}.env" ;;
esac
[ -f "${envfile}" ] || die "找不到目标配置 ${envfile}（参照 deploy/remote/target.example.env 新建）"
# shellcheck disable=SC1090
. "${envfile}"
: "${SSH_HOST:?目标配置缺少 SSH_HOST}"
: "${PUBLIC_HOST:?目标配置缺少 PUBLIC_HOST}"

ssh_opts=(-o ServerAliveInterval=30 -o ConnectTimeout=15)
[ -n "${SSH_PORT:-}" ] && ssh_opts+=(-o "Port=${SSH_PORT}")
if [ -n "${SSH_KEY:-}" ]; then
  key="${SSH_KEY/#\~/${HOME}}"
  ssh_opts+=(-i "${key}" -o IdentitiesOnly=yes)
fi
dest="${SSH_USER:-root}@${SSH_HOST}"
rsh() { ssh "${ssh_opts[@]}" "${dest}" "$@"; }
rcp() { scp -q "${ssh_opts[@]}" "$@"; }

build_bundle() {
  local bundle="$1" arch goarch
  arch="$(rsh uname -m)"
  case "${arch}" in
    x86_64) goarch=amd64 ;;
    aarch64 | arm64) goarch=arm64 ;;
    *) die "不支持的服务器架构 ${arch}" ;;
  esac
  mkdir -p "${bundle}/bin" "${bundle}/web" "${bundle}/units"
  local cache="${root}/.cache/yika-deploy/${goarch}"
  if [ "${SKIP_BUILD:-}" != "1" ] || [ ! -x "${cache}/gost" ]; then
    step "编译服务端（linux/${goarch}）"
    mkdir -p "${cache}"
    local ver
    ver="$(git -C "${root}" describe --tags --always --dirty 2>/dev/null || echo dev)"
    (cd "${root}" && CGO_ENABLED=0 GOOS=linux GOARCH="${goarch}" go build -trimpath -ldflags "-s -w -X main.version=${ver}" -o "${cache}/gost" ./cmd/gost)
    (cd "${root}" && CGO_ENABLED=0 GOOS=linux GOARCH="${goarch}" go build -trimpath -ldflags "-s -w" -o "${cache}/isp-control" ./cmd/isp-control)
    step "构建前端（用户前台、管理后台）"
    (cd "${root}/web" && ${pnpm} install --frozen-lockfile --prefer-offline >/dev/null && ${pnpm} --filter @isp/portal --filter @isp/admin build >/dev/null)
  fi
  cp "${cache}/gost" "${cache}/isp-control" "${bundle}/bin/"
  cp -R "${root}/web/apps/portal/dist" "${bundle}/web/portal"
  cp -R "${root}/web/apps/admin/dist" "${bundle}/web/admin"
  cp "${root}/deploy/isp-control.service" "${root}/deploy/gost-agent.service" "${bundle}/units/"
  cp "${root}/deploy/backup.sh" "${root}/deploy/remote/server.sh" "${bundle}/"
  cp "${envfile}" "${bundle}/target.env"
  if [ -n "${TLS_CERT_FILE:-}" ] || [ -n "${TLS_KEY_FILE:-}" ]; then
    [ -f "${TLS_CERT_FILE/#\~/${HOME}}" ] && [ -f "${TLS_KEY_FILE/#\~/${HOME}}" ] || die "TLS_CERT_FILE / TLS_KEY_FILE 指向的文件不存在"
    mkdir -p "${bundle}/tls"
    cp "${TLS_CERT_FILE/#\~/${HOME}}" "${bundle}/tls/fullchain.pem"
    cp "${TLS_KEY_FILE/#\~/${HOME}}" "${bundle}/tls/key.pem"
  fi
}

ship_and_install() {
  local bundle="$1" restore_file="${2:-}"
  if [ -n "${restore_file}" ]; then
    mkdir -p "${bundle}/restore"
    cp "${restore_file}" "${bundle}/restore/backup.tar.gz"
  fi
  step "上传到 ${dest}"
  local tarball
  tarball="$(dirname "${bundle}")/yika-bundle.tar.gz"
  COPYFILE_DISABLE=1 tar -czf "${tarball}" -C "${bundle}" .
  rsh "rm -rf ${remote_dir} && mkdir -p -m 0700 ${remote_dir}"
  rcp "${tarball}" "${dest}:${remote_dir}/bundle.tar.gz"
  rm -f "${tarball}"
  step "服务器安装"
  rsh "cd ${remote_dir} && tar -xzf bundle.tar.gz && rm bundle.tar.gz && bash ./server.sh install; rc=\$?; rm -rf ${remote_dir}/restore; exit \$rc"
  printf '\n部署完成：\n  用户前台  https://%s:%s\n  管理后台  https://%s:%s\n' "${PUBLIC_HOST}" "${PORTAL_PORT:-36443}" "${PUBLIC_HOST}" "${ADMIN_PORT:-37443}"
}

with_bundle() {
  bundle_work="${BUNDLE_DIR:-$(mktemp -d)}"
  mkdir -p "${bundle_work}/bundle"
  trap 'rm -rf "${bundle_work}"' EXIT
  build_bundle "${bundle_work}/bundle"
  ship_and_install "${bundle_work}/bundle" "${1:-}"
}

case "${cmd}" in
  deploy) with_bundle ;;
  restore)
    [ $# -ge 1 ] && [ -f "$1" ] || die "用法：yika.sh <目标> restore <备份文件 .tar.gz>"
    with_bundle "$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
    ;;
  status) rsh "/usr/local/sbin/yika-server status" ;;
  logs)
    unit="${1:-isp-control}"
    rsh "journalctl -u ${unit} -n ${2:-100} --no-pager"
    ;;
  mail) rsh "/usr/local/sbin/yika-server mail ${1:-5}" ;;
  backup)
    out_dir="${1:-${HOME}/yika-backups/$(basename "${envfile}" .env)}"
    mkdir -p "${out_dir}"
    chmod 0700 "${out_dir}"
    step "服务器生成备份"
    remote_file="$(rsh "/usr/local/sbin/yika-server backup" | tail -1)"
    rcp "${dest}:${remote_file}" "${out_dir}/"
    echo "已下载：${out_dir}/$(basename "${remote_file}")"
    echo "备份里有主密钥与证书私钥，请离线妥善保存，不要放进 Git 或网盘公开目录。"
    ;;
  ssh)
    if [ $# -gt 0 ]; then rsh "$@"; else ssh -t "${ssh_opts[@]}" "${dest}"; fi
    ;;
  *) die "未知命令 ${cmd}（deploy | status | logs | mail | backup | restore | ssh）" ;;
esac
