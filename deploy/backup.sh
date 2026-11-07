#!/usr/bin/env bash
# backup.sh —— 控制面数据库备份（pg_dump 自定义格式），按天保留（示例）。
#
# 用法：ISP_DATABASE_URL=postgres://… ./backup.sh [备份目录]
#   备份目录默认 /var/backups/isp-control；保留天数由 KEEP_DAYS（默认 14）控制。
# 建议用 systemd timer 或 cron 每天执行一次，并把备份目录同步到另一台机器。
#
# 恢复（R-14.6）：
#   pg_restore --clean --if-exists --no-owner -d "$ISP_DATABASE_URL" isp-control-YYYYmmdd-HHMMSS.dump
#   恢复后启动 isp-control；节点上报的累计用量以 GREATEST 合并，不会因旧快照回退（docs/06 §5）。
# 注意：ISP_SECRET_KEY 不在数据库里，必须另行安全备份，否则无法解密接入密码与 TOTP 密钥。
set -euo pipefail

: "${ISP_DATABASE_URL:?需要 ISP_DATABASE_URL}"
dir="${1:-/var/backups/isp-control}"
keep="${KEEP_DAYS:-14}"

umask 077
mkdir -p "$dir"
ts="$(date -u +%Y%m%d-%H%M%S)"
tmp="$dir/.isp-control-$ts.dump.partial"
out="$dir/isp-control-$ts.dump"

pg_dump --format=custom --compress=6 --no-owner --no-privileges --file="$tmp" "$ISP_DATABASE_URL"
# 校验备份可读（列出目录），失败则不保留。
pg_restore --list "$tmp" >/dev/null
mv "$tmp" "$out"
echo "备份完成：$out（$(du -h "$out" | cut -f1)）"

find "$dir" -maxdepth 1 -name 'isp-control-*.dump' -type f -mtime "+$keep" -print -delete
