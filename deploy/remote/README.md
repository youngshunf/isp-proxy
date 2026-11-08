# 一键部署（deploy/remote）

把控制面、节点 agent、用户前台、管理后台装到**一台** Ubuntu 服务器上（22.04 / 24.04，x86_64 或 arm64）。在开发机上执行，脚本会交叉编译、构建前端、打包上传，再在服务器上以 root 完成安装。可以重复执行，用于升级。

## 用法

```bash
cp deploy/remote/target.example.env deploy/remote/targets/hk.env   # targets/ 不进 Git
vi deploy/remote/targets/hk.env                                     # 地址、SSH、端口、节点地域等

deploy/remote/yika.sh hk deploy          # 首次安装或升级
deploy/remote/yika.sh hk status          # 服务状态、健康检查、节点最后上报时间
deploy/remote/yika.sh hk logs gost-agent 200
deploy/remote/yika.sh hk mail 5          # mail.mode = file 时查看验证码邮件
deploy/remote/yika.sh hk backup          # 完整备份下载到 ~/yika-backups/hk/
deploy/remote/yika.sh hk ssh             # 登录服务器
```

`targets/` 整个目录被 Git 忽略。每台服务器的部署信息文档（地址、账号密码，例如 `targets/hk-部署信息.md`）也放在这里。

开发机需要：go（版本见 go.mod）、node 22 + corepack、ssh/scp。服务器需要：root 的 SSH 密钥登录、能访问 apt 源与 apt.postgresql.org。

## 服务器上装了什么

| 组件 | 位置 | 说明 |
| --- | --- | --- |
| PostgreSQL 16 | 本机 5432，只监听回环 | 库与用户都叫 `isp_control` |
| `isp-control` | `127.0.0.1:CONTROL_HTTP_PORT`；节点 mTLS `AGENT_LISTEN_HOST:AGENT_PORT` | systemd `isp-control`，启动即迁移 |
| `gost -agent-config` | 代理端口池 `PROXY_PORT_START-END` | systemd `gost-agent`，节点在首次部署时自动登记 |
| nginx | `PORTAL_PORT`（前台）、`ADMIN_PORT`（后台），HTTPS | 默认站点（80）被移除 |
| ufw | 只放行 SSH、前台、后台、代理端口池 | |
| fail2ban | sshd | 连续输错密码会被暂时封禁 IP |
| 每日备份 | `yika-backup.timer`，03:30，`/var/backups/yika/daily`，保留 14 天 | 只含数据库 |

状态与密钥：`/etc/yika`（0700）。

- `secrets.env`：数据库密码、主密钥 `ISP_SECRET_KEY`、首个管理员初始密码、SMTP 密码。
- `pki/`：节点 mTLS CA 与证书、网站证书。
- `node_id`：本机节点 ID。

这些首次生成后不再覆盖。生成的配置在 `/etc/isp-control`、`/etc/gost-agent`，每次部署都会按目标配置重写，所以不要直接改它们。

## 换服务器

1. 新建目标配置：`targets/new.env`，填新地址。
2. 备份旧服务器：`yika.sh old backup`，得到 `~/yika-backups/old/yika-backup-*.tar.gz`。
3. 恢复到新服务器：`yika.sh new restore ~/yika-backups/old/yika-backup-*.tar.gz`。新服务器必须是全新的。恢复会沿用旧的主密钥、证书与节点 ID，并把节点公共地址改成新的 `PUBLIC_HOST`。
4. 改 DNS 或通知用户新地址后，停掉旧服务器。

注意：备份里有主密钥与证书私钥。不要放进 Git，也不要放进公开网盘。

## 证书

- 网站与 HTTPS 代理入口：不填 `TLS_CERT_FILE` 就用自签证书（有效期 825 天），浏览器第一次访问需要手动信任。有域名后，在目标配置里填正式证书路径（完整链 + 私钥），再 `deploy` 一次。
- 节点 mTLS 证书有效期 1 年。续签方法：删除服务器上 `/etc/yika/pki/agent/` 里的 `server*.pem` 与 `node-*.pem`，再 `deploy` 一次。CA 不要删。

## 邮件

`MAIL_MODE=file` 时不真实发信。注册、找回密码的验证码用 `yika.sh <目标> mail` 查看。

**Gmail（推荐）**：Gmail 发信要用应用专用密码。先给账号开两步验证，再到 https://myaccount.google.com/apppasswords 生成一个 16 位应用专用密码。然后执行：

```bash
deploy/remote/yika.sh hk smtp-gmail 你的地址@gmail.com
```

按提示输入应用专用密码（不回显）后，脚本会依次：

1. 把密码经 ssh 写进服务器 `/etc/yika/secrets.env`；
2. 把目标配置改成 `smtp.gmail.com:587` STARTTLS，发件人设为该 Gmail 地址；
3. 部署；
4. 给该地址发一封“发信测试”邮件。

Gmail 个人账号每天大约能发 500 封。发件人只能是登录的账号本身，或已在 Gmail 里验证过的别名。

**Outlook / Hotmail 个人邮箱不能用**：微软已停用个人邮箱 SMTP 的密码认证，只支持 OAuth2，目前的发信代码不支持。

**其他 SMTP 服务商**：

1. 在目标配置里填 `MAIL_MODE=smtp`、`SMTP_ADDR`、`SMTP_SECURITY`、`SMTP_USERNAME`、`MAIL_FROM`。
2. 执行 `yika.sh <目标> smtp-password` 输入密码。
3. 执行 `deploy`。
4. 用 `yika.sh <目标> mail-test <邮箱>` 验证；发送失败时会打印 SMTP 服务器返回的错误。

## 已知限制

- 出口国家判定靠 `EXIT_COUNTRIES` 静态映射（尚未接 GeoIP）。新录入的上游出口，要先把出口 IP 和国家代码加进去再 `deploy`，否则出口检测判为国家不符，不会分配给用户。
- 端口池只在首次登记节点时生效。之后改目标配置里的端口池不起作用。
