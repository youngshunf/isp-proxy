#!/usr/bin/env bash
# server.sh —— 在目标服务器上以 root 执行（由 yika.sh 上传并调用，不要单独拷到别处用）。
#
#   server.sh install        安装或升级：系统软件、数据库、证书、配置、systemd、nginx、防火墙，然后重启服务
#   server.sh status         服务状态与健康检查
#   server.sh backup         生成完整备份（数据库 + /etc/yika 密钥与证书），打印备份文件路径
#   server.sh mail [N]       查看最近 N 封文件投递的邮件（mail.mode = file 时，验证码在这里）
#   server.sh mail-test <邮箱>        按当前配置发一封测试邮件
#   server.sh set-smtp-password      从标准输入读 SMTP 密码写入 /etc/yika/secrets.env（再 install 一次生效）
#   server.sh geoip-update   下载最新的 DB-IP 免费国家库（每月由 yika-geoip.timer 自动执行）
#
# 同目录需要：target.env、bin/isp-control、bin/gost、web/portal、web/admin、units/、backup.sh；
# 可选 tls/fullchain.pem + tls/key.pem（正式证书）、restore/backup.tar.gz（从备份恢复，只用于全新服务器）。
#
# 状态与密钥都在 /etc/yika（0700），重复执行 install 不会覆盖已生成的密钥、节点与管理员。
set -euo pipefail
# 变量一律写成 ${var}，与仓库其他脚本一致。

here="$(cd "$(dirname "$0")" && pwd)"
state=/etc/yika
secrets="${state}/secrets.env"
pki="${state}/pki"

step() { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die() { printf '错误：%s\n' "$*" >&2; exit 1; }

load_target() {
  [ -f "${here}/target.env" ] || die "缺少 ${here}/target.env"
  # shellcheck disable=SC1091
  . "${here}/target.env"
  : "${PUBLIC_HOST:?target.env 缺少 PUBLIC_HOST}"
  PORTAL_PORT="${PORTAL_PORT:-36443}"
  ADMIN_PORT="${ADMIN_PORT:-37443}"
  CONTROL_HTTP_PORT="${CONTROL_HTTP_PORT:-38080}"
  AGENT_PORT="${AGENT_PORT:-38443}"
  AGENT_LISTEN_HOST="${AGENT_LISTEN_HOST:-127.0.0.1}"
  PROXY_PORT_START="${PROXY_PORT_START:-41000}"
  PROXY_PORT_END="${PROXY_PORT_END:-41999}"
  NODE_NAME="${NODE_NAME:-node-01}"
  DIRECT_REGION_CODE="${DIRECT_REGION_CODE:-}"
  DIRECT_REGION_NAME="${DIRECT_REGION_NAME:-}"
  BRAND="${BRAND:-驿卡}"
  ADMIN_EMAIL="${ADMIN_EMAIL:-admin@example.com}"
  ECHO_URL="${ECHO_URL:-https://api.ipify.org}"
  EXIT_COUNTRIES="${EXIT_COUNTRIES:-}"
  GEOIP="${GEOIP:-dbip}"
  MAIL_MODE="${MAIL_MODE:-file}"
  MAIL_FROM="${MAIL_FROM:-${BRAND} <no-reply@example.invalid>}"
  SMTP_ADDR="${SMTP_ADDR:-}"
  SMTP_SECURITY="${SMTP_SECURITY:-starttls}"
  SMTP_USERNAME="${SMTP_USERNAME:-}"
  SSH_PORT="${SSH_PORT:-22}"
}

is_ip() { [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ || "$1" == *:* ]]; }

psql_q() { runuser -u postgres -- psql -X -v ON_ERROR_STOP=1 -tA "$@"; }
db_q() { psql_q -d isp_control "$@"; }
sql_lit() { printf "'%s'" "${1//\'/\'\'}"; }

geoip_dir=/var/lib/isp-control/geoip
geoip_db="${geoip_dir}/country.mmdb"

# ---------------------------------------------------------------- install

install_packages() {
  step "系统软件（nginx、PostgreSQL 16、防火墙、fail2ban）"
  export DEBIAN_FRONTEND=noninteractive
  local codename
  codename="$(. /etc/os-release && echo "${VERSION_CODENAME}")"
  if [ ! -f /etc/apt/sources.list.d/pgdg.list ]; then
    apt-get update -qq
    apt-get install -y -qq ca-certificates curl gnupg >/dev/null
    install -d /usr/share/postgresql-common/pgdg
    curl -fsSL -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc
    echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt ${codename}-pgdg main" \
      > /etc/apt/sources.list.d/pgdg.list
  fi
  local need=()
  for p in postgresql-16 nginx ufw fail2ban openssl python3 curl; do
    dpkg -s "${p}" >/dev/null 2>&1 || need+=("${p}")
  done
  if [ "${#need[@]}" -gt 0 ]; then
    info "安装 ${need[*]}"
    apt-get update -qq
    apt-get install -y -qq "${need[@]}" >/dev/null
  fi
  timedatectl set-ntp true 2>/dev/null || true
  systemctl enable --now postgresql >/dev/null
  systemctl enable --now fail2ban >/dev/null
}

ensure_users() {
  step "系统用户与目录"
  id isp-control >/dev/null 2>&1 || useradd --system --home-dir /var/lib/isp-control --no-create-home --shell /usr/sbin/nologin isp-control
  id gost >/dev/null 2>&1 || useradd --system --home-dir /var/lib/gost-agent --no-create-home --shell /usr/sbin/nologin gost
  install -d -m 0700 "${state}" "${pki}"
  install -d -m 0750 -g isp-control /etc/isp-control /etc/isp-control/tls
  install -d -m 0750 -g gost /etc/gost-agent
  install -d -m 0755 /srv/yika
  install -d -m 0700 /var/backups/yika
}

restore_state_if_requested() {
  local tarball="${here}/restore/backup.tar.gz"
  [ -f "${tarball}" ] || return 0
  step "从备份恢复密钥与证书"
  [ -f "${secrets}" ] && die "这台服务器已经部署过（存在 ${secrets}），恢复只用于全新服务器"
  local tmp
  tmp="$(mktemp -d)"
  tar -xzf "${tarball}" -C "${tmp}"
  [ -f "${tmp}/etc-yika.tar" ] && [ -f "${tmp}/db.dump" ] || die "备份文件不完整（需要 etc-yika.tar 与 db.dump）"
  tar -xf "${tmp}/etc-yika.tar" -C /etc
  chmod 0700 "${state}"
  RESTORE_DUMP="${tmp}/db.dump"
  info "已恢复 /etc/yika（主密钥、证书、节点 ID）"
}

ensure_secrets() {
  step "密钥（只在首次部署时生成）"
  if [ ! -f "${secrets}" ]; then
    umask 077
    cat > "${secrets}" <<EOF
# 驿卡服务器密钥（首次部署生成，0600，只在本机与离线备份中保存，不要提交到 Git）。
# 主密钥丢失将无法解密接入密码、上游密码与 TOTP 密钥；换服务器用 yika.sh <目标> backup / restore 迁移。
DB_PASSWORD=$(openssl rand -hex 24)
ISP_SECRET_KEY=$(openssl rand -hex 32)
# 首个管理员的初始密码（登录后台后请在验证器绑定 TOTP；可在后台修改密码后删除这一行）。
ADMIN_PASSWORD=$(openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | cut -c1-20)
# SMTP 密码（mail.mode = smtp 时填写，然后重新 deploy）。
SMTP_PASSWORD=
EOF
    umask 022
    info "已生成 ${secrets}"
  else
    info "沿用 ${secrets}"
  fi
  chmod 0600 "${secrets}"
  # shellcheck disable=SC1090
  . "${secrets}"
}

ensure_database() {
  step "数据库"
  local exists
  exists="$(psql_q -c "SELECT 1 FROM pg_roles WHERE rolname = 'isp_control'")"
  if [ "${exists}" != "1" ]; then
    psql_q -c "CREATE ROLE isp_control LOGIN PASSWORD $(sql_lit "${DB_PASSWORD}")" >/dev/null
  else
    psql_q -c "ALTER ROLE isp_control LOGIN PASSWORD $(sql_lit "${DB_PASSWORD}")" >/dev/null
  fi
  exists="$(psql_q -c "SELECT 1 FROM pg_database WHERE datname = 'isp_control'")"
  if [ "${exists}" != "1" ]; then
    psql_q -c "CREATE DATABASE isp_control OWNER isp_control ENCODING 'UTF8' TEMPLATE template0" >/dev/null
    info "已创建数据库 isp_control"
  fi
  if [ -n "${RESTORE_DUMP:-}" ]; then
    local tables
    tables="$(db_q -c "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'")"
    [ "${tables}" = "0" ] || die "数据库 isp_control 不是空的，拒绝恢复"
    info "恢复数据库备份"
    chmod 0644 "${RESTORE_DUMP}"
    chmod 0755 "$(dirname "${RESTORE_DUMP}")"
    runuser -u postgres -- pg_restore --no-owner --role=isp_control -d isp_control "${RESTORE_DUMP}"
  fi
}

install_files() {
  step "程序与网页"
  install -m 0755 "${here}/bin/isp-control" /usr/local/bin/isp-control
  install -m 0755 "${here}/bin/gost" /usr/local/bin/gost
  install -m 0755 "${here}/backup.sh" /usr/local/sbin/yika-db-backup
  install -m 0755 "${here}/server.sh" /usr/local/sbin/yika-server
  install -m 0644 "${here}/target.env" "${state}/target.env"
  local app
  for app in portal admin; do
    rm -rf "/srv/yika/${app}.new"
    cp -r "${here}/web/${app}" "/srv/yika/${app}.new"
    chmod -R a+rX "/srv/yika/${app}.new"
    rm -rf "/srv/yika/${app}.old"
    [ -d "/srv/yika/${app}" ] && mv "/srv/yika/${app}" "/srv/yika/${app}.old"
    mv "/srv/yika/${app}.new" "/srv/yika/${app}"
    rm -rf "/srv/yika/${app}.old"
  done
  info "$(/usr/local/bin/gost -V 2>/dev/null | head -1 || true)"
}

yaml_str() { # JSON 字符串字面量即合法的 YAML 双引号字符串
  python3 -c 'import json,sys; print(json.dumps(sys.argv[1], ensure_ascii=False))' "$1"
}

write_control_config() {
  step "控制面配置"
  local countries="" pair ip cc
  for pair in ${EXIT_COUNTRIES}; do
    ip="${pair%%=*}"; cc="${pair#*=}"
    countries="${countries}
    $(yaml_str "${ip}"): $(yaml_str "${cc}")"
  done
  [ -n "${countries}" ] || countries=" {}"
  local geoip_line=""
  [ "${GEOIP}" = "off" ] || geoip_line="
  geoip_db: ${geoip_db}"
  local mail_extra=""
  if [ "${MAIL_MODE}" = "smtp" ]; then
    mail_extra="
  smtp_addr: $(yaml_str "${SMTP_ADDR}")
  smtp_security: $(yaml_str "${SMTP_SECURITY}")
  smtp_username: $(yaml_str "${SMTP_USERNAME}")"
  fi
  cat > /etc/isp-control/isp-control.yaml <<EOF
# 由 deploy/remote/server.sh 生成，改目标配置后重新 deploy；直接改这里会在下次部署时被覆盖。
listen: 127.0.0.1:${CONTROL_HTTP_PORT}
secure_cookies: true
trusted_proxies:
  - 127.0.0.1
  - ::1
totp_issuer: $(yaml_str "${BRAND}管理后台")
agent:
  listen: ${AGENT_LISTEN_HOST}:${AGENT_PORT}
  ca_file: /etc/isp-control/tls/ca.pem
  cert_file: /etc/isp-control/tls/server.pem
  key_file: /etc/isp-control/tls/server-key.pem
  long_poll: 25s
core:
  echo_url: $(yaml_str "${ECHO_URL}")
  exit_countries:${countries}${geoip_line}
  shared_exit_default_capacity: 3
mail:
  mode: ${MAIL_MODE}
  file_dir: /var/lib/isp-control/mail
  from: $(yaml_str "${MAIL_FROM}")
  brand: $(yaml_str "${BRAND}")
  site_url: $(yaml_str "https://${PUBLIC_HOST}:${PORTAL_PORT}")${mail_extra}
log:
  level: info
  format: json
EOF
  umask 077
  cat > /etc/isp-control/isp-control.env <<EOF
ISP_DATABASE_URL=postgres://isp_control:${DB_PASSWORD}@127.0.0.1:5432/isp_control?sslmode=disable
ISP_SECRET_KEY=${ISP_SECRET_KEY}
ISP_SMTP_PASSWORD="${SMTP_PASSWORD}"
EOF
  umask 022
  chown root:isp-control /etc/isp-control/isp-control.yaml /etc/isp-control/isp-control.env
  chmod 0640 /etc/isp-control/isp-control.yaml /etc/isp-control/isp-control.env
}

control_cli() {
  ( set -a; . /etc/isp-control/isp-control.env; set +a
    exec /usr/local/bin/isp-control "$@" )
}

migrate_db() {
  step "数据库迁移"
  control_cli migrate -config /etc/isp-control/isp-control.yaml | sed 's/^/    /'
}

ensure_node() {
  step "本机节点"
  local id=""
  [ -f "${state}/node_id" ] && id="$(cat "${state}/node_id")"
  if [ -n "${id}" ] && [ "$(db_q -c "SELECT 1 FROM nodes WHERE id = $(sql_lit "${id}")")" = "1" ]; then
    db_q -c "UPDATE nodes SET public_host = $(sql_lit "${PUBLIC_HOST}") WHERE id = $(sql_lit "${id}") AND public_host <> $(sql_lit "${PUBLIC_HOST}")" >/dev/null
    info "沿用节点 ${id}（公共地址 ${PUBLIC_HOST}）"
  else
    local dcc="NULL" dreg="NULL"
    if [ -n "${DIRECT_REGION_CODE}" ] && [ -n "${DIRECT_REGION_NAME}" ]; then
      dcc="$(sql_lit "$(echo "${DIRECT_REGION_CODE}" | tr 'A-Z' 'a-z')")"
      dreg="$(sql_lit "${DIRECT_REGION_NAME}")"
    fi
    id="$(db_q -c "WITH n AS (
        INSERT INTO nodes (name, public_host, port_range_start, port_range_end, direct_country_code, direct_region)
        VALUES ($(sql_lit "${NODE_NAME}"), $(sql_lit "${PUBLIC_HOST}"), ${PROXY_PORT_START}, ${PROXY_PORT_END}, ${dcc}, ${dreg})
        RETURNING id),
      p AS (INSERT INTO ports (node_id, port) SELECT n.id, g FROM n, generate_series(${PROXY_PORT_START}, ${PROXY_PORT_END}) g)
      SELECT id FROM n" | head -1)"
    [ -n "${id}" ] || die "登记节点失败"
    echo "${id}" > "${state}/node_id"
    info "已登记节点 ${id}（${NODE_NAME}，端口池 ${PROXY_PORT_START}-${PROXY_PORT_END}）"
  fi
  NODE_ID="${id}"
  # 端口池以数据库为准（首次登记后不随目标配置变化）。
  read -r PROXY_PORT_START PROXY_PORT_END < <(db_q -F ' ' -c "SELECT port_range_start, port_range_end FROM nodes WHERE id = $(sql_lit "${NODE_ID}")")
}

ensure_certs() {
  step "证书"
  # 节点 mTLS：CA、控制面服务端证书、节点客户端证书（有效期 1 年，已存在则沿用；到期前删除对应文件后重新 deploy 即续签）。
  local hosts=(-host 127.0.0.1 -host localhost)
  [ "${PUBLIC_HOST}" != "127.0.0.1" ] && hosts+=(-host "${PUBLIC_HOST}")
  /usr/local/bin/isp-control dev-certs -dir "${pki}/agent" -node "${NODE_ID}" "${hosts[@]}" >/dev/null
  install -m 0640 -o root -g isp-control "${pki}/agent/ca.pem" /etc/isp-control/tls/ca.pem
  install -m 0640 -o root -g isp-control "${pki}/agent/server.pem" /etc/isp-control/tls/server.pem
  install -m 0640 -o root -g isp-control "${pki}/agent/server-key.pem" /etc/isp-control/tls/server-key.pem
  install -m 0640 -o root -g gost "${pki}/agent/ca.pem" /etc/gost-agent/ca.pem
  install -m 0640 -o root -g gost "${pki}/agent/node-${NODE_ID}.pem" /etc/gost-agent/node.pem
  install -m 0640 -o root -g gost "${pki}/agent/node-${NODE_ID}-key.pem" /etc/gost-agent/node-key.pem

  # 网站与 HTTPS 代理入口证书：有正式证书用正式的，否则自签（地址变化时重签）。
  install -d -m 0700 "${pki}/web"
  if [ -f "${here}/tls/fullchain.pem" ] && [ -f "${here}/tls/key.pem" ]; then
    install -m 0600 "${here}/tls/fullchain.pem" "${pki}/web/fullchain.pem"
    install -m 0600 "${here}/tls/key.pem" "${pki}/web/key.pem"
    rm -f "${pki}/web/self-signed"
    info "使用上传的正式证书"
  else
    local want san
    if is_ip "${PUBLIC_HOST}"; then san="IP:${PUBLIC_HOST}"; else san="DNS:${PUBLIC_HOST}"; fi
    want="${san}"
    if [ ! -f "${pki}/web/fullchain.pem" ] || { [ -f "${pki}/web/self-signed" ] && [ "$(cat "${pki}/web/self-signed")" != "${want}" ]; }; then
      openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 825 \
        -subj "/CN=${PUBLIC_HOST}" -addext "subjectAltName=${san}" \
        -addext "extendedKeyUsage=serverAuth" -addext "keyUsage=digitalSignature" \
        -keyout "${pki}/web/key.pem" -out "${pki}/web/fullchain.pem" 2>/dev/null
      echo "${want}" > "${pki}/web/self-signed"
      info "已生成自签证书（${san}，825 天）"
    else
      info "沿用网站证书"
    fi
  fi
  install -d -m 0750 -g www-data /etc/nginx/yika
  install -m 0644 "${pki}/web/fullchain.pem" /etc/nginx/yika/fullchain.pem
  install -m 0640 -o root -g www-data "${pki}/web/key.pem" /etc/nginx/yika/key.pem
  install -m 0640 -o root -g gost "${pki}/web/fullchain.pem" /etc/gost-agent/entry.pem
  install -m 0640 -o root -g gost "${pki}/web/key.pem" /etc/gost-agent/entry-key.pem
}

write_agent_config() {
  step "节点 agent 配置"
  cat > /etc/gost-agent/agent.yaml <<EOF
# 由 deploy/remote/server.sh 生成（字段说明见 deploy/agent.example.yaml）。
node_id: ${NODE_ID}
control:
  url: https://127.0.0.1:${AGENT_PORT}
  ca_file: /etc/gost-agent/ca.pem
  cert_file: /etc/gost-agent/node.pem
  key_file: /etc/gost-agent/node-key.pem
data_dir: /var/lib/gost-agent
entry:
  bind_host: ""
  cert_file: /etc/gost-agent/entry.pem
  key_file: /etc/gost-agent/entry-key.pem
  cert_reload_interval: 1m
egress:
  allow_cidrs: []
  allow_ports: []
upstream_tls:
  ca_file: ""
  insecure_skip_verify: false
probe:
  echo_url: $(yaml_str "${ECHO_URL}")
  timeout: 15s
  concurrency: 4
report_interval: 5s
flush_interval: 1s
EOF
  chown root:gost /etc/gost-agent/agent.yaml
  chmod 0640 /etc/gost-agent/agent.yaml
}

write_units() {
  step "systemd 服务"
  install -m 0644 "${here}/units/isp-control.service" /etc/systemd/system/isp-control.service
  install -m 0644 "${here}/units/gost-agent.service" /etc/systemd/system/gost-agent.service
  cat > /etc/systemd/system/yika-backup.service <<'EOF'
[Unit]
Description=驿卡数据库每日备份
After=postgresql.service

[Service]
Type=oneshot
EnvironmentFile=/etc/isp-control/isp-control.env
Environment=KEEP_DAYS=14
ExecStart=/usr/local/sbin/yika-db-backup /var/backups/yika/daily
EOF
  cat > /etc/systemd/system/yika-backup.timer <<'EOF'
[Unit]
Description=驿卡数据库每日备份

[Timer]
OnCalendar=*-*-* 03:30:00
Persistent=true
RandomizedDelaySec=10m

[Install]
WantedBy=timers.target
EOF
  if [ "${GEOIP}" = "off" ]; then
    systemctl disable --now yika-geoip.timer >/dev/null 2>&1 || true
    rm -f /etc/systemd/system/yika-geoip.service /etc/systemd/system/yika-geoip.timer
  else
    cat > /etc/systemd/system/yika-geoip.service <<'EOF'
[Unit]
Description=驿卡 IP 归属地库每月更新
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/yika-server geoip-update
EOF
    cat > /etc/systemd/system/yika-geoip.timer <<'EOF'
[Unit]
Description=驿卡 IP 归属地库每月更新

[Timer]
OnCalendar=*-*-03 04:00:00
Persistent=true
RandomizedDelaySec=1h

[Install]
WantedBy=timers.target
EOF
  fi
  systemctl daemon-reload
  systemctl enable isp-control gost-agent yika-backup.timer >/dev/null 2>&1
  systemctl start yika-backup.timer
  if [ "${GEOIP}" != "off" ]; then
    systemctl enable --now yika-geoip.timer >/dev/null 2>&1
  fi
}

# DB-IP IP to Country Lite：免费、无需注册，按月发布，许可 CC BY 4.0（需署名 DB-IP，管理后台页脚已署名）。
# 先取本月，取不到（月初尚未发布）再取上月。下载失败不动现有库文件；控制面一分钟内自动换用新文件。
cmd_geoip_update() {
  local tmp month url ok=""
  install -d -m 0755 -o isp-control -g isp-control "${geoip_dir}"
  tmp="$(mktemp "${geoip_dir}/.download.XXXXXX")"
  for month in "$(date -u +%Y-%m)" "$(date -u -d "$(date -u +%Y-%m-15) -1 month" +%Y-%m)"; do
    url="https://download.db-ip.com/free/dbip-country-lite-${month}.mmdb.gz"
    if curl -fsSL --retry 2 --max-time 300 "${url}" 2>/dev/null | gunzip -c > "${tmp}" 2>/dev/null \
      && [ "$(stat -c %s "${tmp}")" -gt 1000000 ] && tail -c 131072 "${tmp}" | grep -aq 'MaxMind.com'; then
      ok="${month}"
      break
    fi
  done
  if [ -z "${ok}" ]; then
    rm -f "${tmp}"
    printf '警告：IP 归属地库下载失败（download.db-ip.com），沿用现有文件\n' >&2
    return 1
  fi
  chmod 0644 "${tmp}"
  chown isp-control:isp-control "${tmp}"
  mv -f "${tmp}" "${geoip_db}"
  info "IP 归属地库已更新：DB-IP Country Lite ${ok}（${geoip_db}）"
}

ensure_geoip() {
  [ "${GEOIP}" = "off" ] && return 0
  step "IP 归属地库"
  # 文件不存在或超过 40 天没更新时立即下载；失败只警告，出口国家仍可用 EXIT_COUNTRIES 手工指定。
  if [ -s "${geoip_db}" ] && [ -z "$(find "${geoip_db}" -mtime +40 2>/dev/null)" ]; then
    info "已有 ${geoip_db}（$(date -u -r "${geoip_db}" +%F) 更新），每月 3 日自动更新"
  else
    cmd_geoip_update || true
  fi
}

write_nginx() {
  step "nginx（前台 :${PORTAL_PORT}，后台 :${ADMIN_PORT}）"
  rm -f /etc/nginx/sites-enabled/default
  local tls='
    ssl_certificate     /etc/nginx/yika/fullchain.pem;
    ssl_certificate_key /etc/nginx/yika/key.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:yika:10m;
    # 用 http:// 访问 HTTPS 端口时跳转到 https://
    error_page 497 =301 https://$host:$server_port$request_uri;
    add_header X-Content-Type-Options nosniff always;
    add_header Referrer-Policy same-origin always;
    add_header X-Frame-Options DENY always;
    server_tokens off;'
  local proxy='
        proxy_pass http://yika_control;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection "";
        client_max_body_size 1m;'
  cat > /etc/nginx/sites-available/yika.conf <<EOF
# 由 deploy/remote/server.sh 生成。
upstream yika_control {
    server 127.0.0.1:${CONTROL_HTTP_PORT};
    keepalive 16;
}

# 用户前台
server {
    listen ${PORTAL_PORT} ssl http2;
    listen [::]:${PORTAL_PORT} ssl http2;
    server_name _;
${tls}
    root /srv/yika/portal;
    index index.html;
    location /api/ {${proxy}
    }
    location /admin-api/ { return 404; }
    location /assets/ {
        expires 1y;
        add_header Cache-Control "public, immutable";
        try_files \$uri =404;
    }
    location / {
        add_header Cache-Control "no-cache";
        try_files \$uri \$uri/ /index.html;
    }
}

# 管理后台
server {
    listen ${ADMIN_PORT} ssl http2;
    listen [::]:${ADMIN_PORT} ssl http2;
    server_name _;
${tls}
    root /srv/yika/admin;
    index index.html;
    location /admin-api/ {${proxy}
    }
    location /api/ { return 404; }
    location /assets/ {
        expires 1y;
        add_header Cache-Control "public, immutable";
        try_files \$uri =404;
    }
    location / {
        add_header Cache-Control "no-cache";
        try_files \$uri \$uri/ /index.html;
    }
}
EOF
  ln -sf /etc/nginx/sites-available/yika.conf /etc/nginx/sites-enabled/yika.conf
  nginx -t 2>&1 | sed 's/^/    /'
  systemctl enable nginx >/dev/null 2>&1
  systemctl reload nginx 2>/dev/null || systemctl restart nginx
}

write_firewall() {
  step "防火墙（ufw）"
  ufw default deny incoming >/dev/null
  ufw default allow outgoing >/dev/null
  # 放行 sshd 实际监听的全部端口与目标配置里的 SSH 端口，避免把自己关在外面。
  local p
  for p in "${SSH_PORT}" $(ss -Htlnp 2>/dev/null | awk '/"sshd"/ {n = split($4, a, ":"); print a[n]}' | sort -u); do
    ufw allow "${p}/tcp" comment 'ssh' >/dev/null
  done
  ufw allow "${PORTAL_PORT}/tcp" comment 'yika portal' >/dev/null
  ufw allow "${ADMIN_PORT}/tcp" comment 'yika admin' >/dev/null
  ufw allow "${PROXY_PORT_START}:${PROXY_PORT_END}/tcp" comment 'yika proxy ports' >/dev/null
  if [ "${AGENT_LISTEN_HOST}" != "127.0.0.1" ]; then
    ufw allow "${AGENT_PORT}/tcp" comment 'yika agent mTLS' >/dev/null
  else
    ufw delete allow "${AGENT_PORT}/tcp" >/dev/null 2>&1 || true
  fi
  ufw --force enable >/dev/null
  ufw status | sed 's/^/    /'
}

ensure_admin() {
  step "管理员"
  if [ "$(db_q -c "SELECT count(*) FROM admins")" != "0" ]; then
    info "已有管理员，跳过"
    return
  fi
  ISP_ADMIN_PASSWORD="${ADMIN_PASSWORD}" control_cli admin create -config /etc/isp-control/isp-control.yaml \
    -email "${ADMIN_EMAIL}" -name "管理员" -role admin | sed 's/^/    /'
}

wait_http() { # url 秒数
  local i
  for ((i = 0; i < $2; i++)); do
    curl -fsk -o /dev/null "$1" && return 0
    sleep 1
  done
  return 1
}

restart_services() {
  step "启动服务"
  systemctl restart isp-control
  wait_http "http://127.0.0.1:${CONTROL_HTTP_PORT}/healthz" 30 || { journalctl -u isp-control -n 40 --no-pager; die "isp-control 没有就绪"; }
  info "isp-control 就绪"
  systemctl restart gost-agent
  sleep 3
  systemctl is-active --quiet gost-agent || { journalctl -u gost-agent -n 40 --no-pager; die "gost-agent 没有启动"; }
  info "gost-agent 运行中"
}

cmd_install() {
  load_target
  install_packages
  ensure_users
  restore_state_if_requested
  ensure_secrets
  ensure_database
  install_files
  ensure_geoip
  write_control_config
  migrate_db
  ensure_node
  ensure_certs
  write_agent_config
  write_units
  ensure_admin
  restart_services
  write_nginx
  write_firewall
  cmd_status
}

# ---------------------------------------------------------------- 其他命令

cmd_status() {
  [ -f "${state}/target.env" ] && here="${state}"
  load_target
  step "状态"
  local s
  for s in postgresql isp-control gost-agent nginx fail2ban yika-backup.timer yika-geoip.timer; do
    printf '    %-18s %s\n' "${s}" "$(systemctl is-active "${s}" 2>/dev/null || true)"
  done
  printf '    %-18s %s\n' "healthz" "$(curl -fsk "http://127.0.0.1:${CONTROL_HTTP_PORT}/healthz" 2>/dev/null || echo 失败)"
  printf '    %-18s %s\n' "前台" "$(curl -sk -o /dev/null -w '%{http_code}' "https://127.0.0.1:${PORTAL_PORT}/")"
  printf '    %-18s %s\n' "后台" "$(curl -sk -o /dev/null -w '%{http_code}' "https://127.0.0.1:${ADMIN_PORT}/")"
  if [ -f "${state}/node_id" ]; then
    printf '    %-18s %s\n' "节点" "$(db_q -F ' ' -c "SELECT id, name, status, 'last_seen=' || coalesce(to_char(last_seen_at AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD HH24:MI:SS'), '从未') FROM nodes WHERE id = '$(cat "${state}/node_id")'")"
  fi
}

cmd_backup() {
  local ts out tmp
  ts="$(date -u +%Y%m%d-%H%M%S)"
  out="/var/backups/yika/yika-backup-${ts}.tar.gz"
  tmp="$(mktemp -d)"
  chown postgres "${tmp}"
  runuser -u postgres -- pg_dump --format=custom --compress=6 --no-owner --no-privileges -d isp_control -f "${tmp}/db.dump"
  runuser -u postgres -- pg_restore --list "${tmp}/db.dump" >/dev/null
  tar -cf "${tmp}/etc-yika.tar" -C /etc yika
  umask 077
  tar -czf "${out}" -C "${tmp}" db.dump etc-yika.tar
  rm -rf "${tmp}"
  # 只保留最近 10 份完整备份
  ls -1t /var/backups/yika/yika-backup-*.tar.gz 2>/dev/null | tail -n +11 | xargs -r rm -f
  echo "${out}"
}

cmd_mail() {
  local n="${1:-5}"
  python3 - "${n}" <<'EOF'
import email, email.policy, glob, os, sys
files = sorted(glob.glob('/var/lib/isp-control/mail/*.eml'), key=os.path.getmtime, reverse=True)[: int(sys.argv[1])]
if not files:
    print('没有邮件（mail.mode = file 时邮件写在 /var/lib/isp-control/mail）')
for f in reversed(files):
    m = email.message_from_binary_file(open(f, 'rb'), policy=email.policy.default)
    print('=' * 60)
    print('时间：', m['Date'])
    print('收件人：', m['To'])
    print('主题：', m['Subject'])
    print('-' * 60)
    print(m.get_body(('plain',)).get_content().strip())
EOF
}

# 从标准输入读一行，写入 secrets.env 的 SMTP_PASSWORD（不出现在命令行与日志里）；生效需要再部署一次。
cmd_set_smtp_password() {
  [ -f "${secrets}" ] || die "还没部署过（缺少 ${secrets}）"
  local pw
  IFS= read -r pw || true
  [ -n "${pw}" ] || die "没有读到密码"
  case "${pw}" in *'"'* | *'\'* | *'$'* | *'`'*) die "密码里不能有 \" \\ \$ \` 字符" ;; esac
  PW="${pw}" python3 -I - "${secrets}" <<'EOF'
import os, sys
p = sys.argv[1]
lines = open(p, encoding="utf-8").read().splitlines()
out, done = [], False
for l in lines:
    if l.startswith("SMTP_PASSWORD="):
        out.append('SMTP_PASSWORD="%s"' % os.environ["PW"]); done = True
    else:
        out.append(l)
if not done:
    out.append('SMTP_PASSWORD="%s"' % os.environ["PW"])
open(p, "w", encoding="utf-8").write("\n".join(out) + "\n")
EOF
  chmod 0600 "${secrets}"
  echo "已写入 ${secrets} 的 SMTP_PASSWORD"
}

cmd_mail_test() {
  [ $# -ge 1 ] || die "用法：server.sh mail-test <收件人邮箱>"
  control_cli mail-test -config /etc/isp-control/isp-control.yaml -to "$1"
}

[ "$(id -u)" = "0" ] || die "需要 root"
case "${1:-install}" in
  install) cmd_install ;;
  status) cmd_status ;;
  backup) cmd_backup ;;
  mail) shift; cmd_mail "$@" ;;
  mail-test) shift; cmd_mail_test "$@" ;;
  set-smtp-password) cmd_set_smtp_password ;;
  geoip-update) cmd_geoip_update ;;
  *) die "未知命令 $1（install | status | backup | mail [N] | mail-test <邮箱> | set-smtp-password | geoip-update）" ;;
esac
