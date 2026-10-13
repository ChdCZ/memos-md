# memos-md 使用与发布指南

这份文档面向「把这个仓库放到自己的 GitHub 上、然后长期使用和维护它」的场景。里面所有命令都在一台 Debian 13
服务器（Docker 29.9.0）和一台 Windows 机器上实际执行过，凡是没验证过的步骤都会明确标注。

- 想先了解「这个 fork 改了什么」→ 看 [FORK.md](FORK.md)
- 想直接把镜像跑起来 → 跳到 [三、部署](#三部署)
- 想发布到 GitHub / 打 tag 出二进制 → 跳到 [二、发布到 GitHub](#二发布到-github)
- 遇到问题 → [六、故障排查](#六故障排查)
---

## 一、仓库结构速览

```
master    ── 上游 usememos/memos v0.31.0 原样提交（保留完整历史，方便以后 rebase）
memos-md  ── 本 fork 的改动，3 个提交
tag: v0.31.0-memos-md.3 ── 主机实测过的版本
```

改动范围很小，一共 24 个文件、约 2800 行，其中真正动到上游代码的只有 6 个：

| 路径 | 说明 |
| --- | --- |
| `core/memofile/` | **新增**。front matter 格式、镜像、启动同步、回归测试 |
| `store/store.go` | 新增 `MemoMirror` 接口 + `SetMemoMirror` + 一个辅助方法 |
| `store/memo.go` | `CreateMemo` / `UpdateMemo` / `DeleteMemo` 写成功后回调镜像 |
| `store/memo_attachment.go` | `ApplyMemoMutation` 提交后回调镜像 |
| `store/memo_delete_policy.go` | `DeleteMemoWithPolicy` 提交后回调镜像 |
| `store/user.go` | 首个账号创建后发一次通知（用于导入等待中的文件） |
| `cmd/memos/main.go` | `--memo-files` 开关、启动同步、优雅关闭 |
| `Dockerfile` / `docker-compose.memofiles.yml` | **新增**。上游根目录没有 Dockerfile，这两个负责构建和部署 |
| `cmd/memos-build/` | **新增**。一条命令构建「前端 + 二进制」的本地辅助工具 |
| `.github/workflows/memos-md-build.yml` | **新增**。跑测试，并在打 tag 时产出四个平台的二进制 |
| `FORK.md` / `GUIDE.md` / `examples/` | 文档和两条示例笔记 |

`server/`、`web/`、`proto/`、三个数据库驱动**一行都没改**。

---

## 二、发布到 GitHub

### 2.1 准备 git

Windows 上如果没装 git，两种方式：

```powershell
# 方式一：winget 安装（需要交互确认）
winget install --id Git.Git -e

# 方式二：便携版，免安装免管理员（我测试时用的就是这个）
#   下载 https://github.com/git-for-windows/git/releases 里的 PortableGit-*-64-bit.7z.exe
#   解压后直接用 <解压目录>\cmd\git.exe
```

### 2.2 在 GitHub 上 fork

打开 <https://github.com/usememos/memos> → 右上角 **Fork** → 仓库名建议填 `memos-md`。

> 为什么要 fork 而不是新建空仓库：fork 保留了和上游的 commit 血缘，GitHub 页面会显示
> "forked from usememos/memos"，以后 `git fetch upstream` + `git rebase` 就能跟上 memos 的新版本。
> 如果是新建空仓库再上传，就只能手动合上游的改动了。

### 2.3 把你的改动推上去

本地仓库已经就绪（`master` = 上游，`memos-md` = 改动），只需要加 remote 再推：

```bash
# 解压归档后进入仓库
tar xzf memos-md-v0.31.0-fork-with-git.tar.gz
cd memos-md-git

# 换成你自己的仓库地址
git remote add origin https://github.com/<你的用户名>/memos-md.git

git push -u origin master                      # 让 fork 的分支和上游对齐
git push origin memos-md                       # 你的改动分支
git push origin v0.31.0-memos-md.3             # 版本 tag
```

推送时 GitHub 会要求认证。用 **Personal Access Token**（代替密码）：

1. <https://github.com/settings/tokens> → Generate new token (classic) → 勾选 `repo` 和 `workflow`
2. 推送时用户名填 GitHub 用户名，密码粘贴 token
3. 想免输入：`git config --global credential.helper manager`（Windows 会弹图形登录框）

> `workflow` 权限是必须的：仓库里有 `.github/workflows/`，没有这个 scope 推不上去。

### 2.4 让改动一眼可见

推送完在 GitHub 上开一个 PR 给自己看：`base: <你>/memos-md:master` ← `compare: memos-md`。
PR 页面就是你这份 23 文件的 diff，合不合并都行，纯粹方便自己 review 和以后回看。

### 2.5 打 tag 自动出二进制

仓库里已带 `.github/workflows/memos-md-build.yml`：先在 CI 跑 `core/memofile` 的测试，然后在 `v*` tag 上构建
linux/amd64、linux/arm64、darwin/arm64、windows/amd64 四个二进制并挂到 Release。

```bash
git tag -a v0.31.0-memos-md.3 -m "memos v0.31.0 + markdown-file storage"
git push origin v0.31.0-memos-md.3
```

几分钟后到仓库的 **Releases** 页面下载即可。二进制是**自带前端**的：`./memos-md-linux-amd64 --memo-files` 直接能跑。

> 这个 workflow 是照着上游 `release.yml` 的结构写的，但**我没有 GitHub Actions 环境实测过**。
> 如果第一次跑失败，大概率是 `go-version-file: go.mod`（要求 go 1.27.2）或 pnpm 版本，看报错改那两行即可。

### 2.6 跟上上游更新

```bash
git remote add upstream https://github.com/usememos/memos.git   # 只需一次
git fetch upstream --tags
git checkout memos-md
git rebase upstream/main        # 或某个 release tag
git push --force-with-lease origin memos-md
```

改动的 6 个文件都贴着「写成功之后调用一次」的位置，上游重构 store 层时冲突会集中在这几行，好解决。

### 2.7 顺手打开的仓库设置

| 位置 | 建议 |
| --- | --- |
| Settings → General → Default branch | 改成 `memos-md`，别人进来看到的就是带改动的版本 |
| Settings → General → Features → Issues | 打开，方便记需求 |
| About → Description | 例：`memos + markdown files on disk: every memo is a .md file in data/memos/` |
| About → Topics | `memos` `markdown` `self-hosted` `docker` `notes` |
| Settings → Actions → General | 新 fork 的仓库通常默认不允许运行 workflow，第一次 push tag 前确认这里是 enabled |

---

## 三、部署

### 3.1 Docker（推荐，镜像自带前端）

```bash
git clone https://github.com/<你>/memos-md.git && cd memos-md
mkdir -p data

# 让笔记属于你自己（而不是默认的 uid 10001），否则改文件要 sudo
MEMOS_UID=$(id -u) MEMOS_GID=$(id -g) \
  docker compose -f docker-compose.memofiles.yml up -d --build

# 打开 http://<服务器IP>:5230 ，创建管理员账号，开始写
```

首次构建在 2 GB 内存的机器上大约 5–10 分钟（要装 npm 依赖 + 编译 Go）。之后改代码重建只要 1 分钟左右，
因为 Go 模块和 pnpm store 都走了 BuildKit 缓存。

### 3.2 只用 Go（镜像里已有构建好的前端）

```bash
go run ./cmd/memos --memo-files --port 5230 --data ./data
```

源码 clone 下来时 `server/frontend/dist` 里只有上游的占位页（真正的 SPA 由 CI 或 Dockerfile 生成），
所以要用到界面时先构建一次前端：

```bash
cd web && pnpm install && pnpm release && cd ..   # 需要 Node 24 + pnpm 11
go build -o memos-md ./cmd/memos
./memos-md --memo-files --port 5230 --data ./data
```

或者用仓库里的辅助工具一条命令搞定（会自动跑上面两步）：

```bash
go run cmd/memos-build/main.go -o memos-md
```

### 3.3 systemd 常驻（可选）

Docker 的 `restart: unless-stopped` 已经保证开机自启，只想要进程级别的自启可以不管这节。
如果不用 Docker，写个 unit：

```ini
# /etc/systemd/system/memos-md.service
[Unit]
Description=memos-md
After=network.target

[Service]
User=xxqq
WorkingDirectory=/home/xxqq/memos-md
ExecStart=/home/xxqq/memos-md/memos-md --memo-files --port 5230 --data /home/xxqq/memos-md/data
Restart=always

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now memos-md
```

### 3.4 放公网 / 反代

实例默认监听 `0.0.0.0:5230`，同局域网直接访问即可。要放公网至少做三件事：

1. **设 `MEMOS_INSTANCE_URL`** 为对外地址（`https://memos.example.com`），否则分享链接、SSO 回调会错
2. **用 Caddy/Nginx 反代并上 HTTPS**，同时把 5230 从公网防火墙里关掉
3. **注意注册开关**：memos 允许注册时任何人拿到账号就能看到 `PROTECTED` 笔记。单人使用建议
   Settings → Instance → 关闭注册，只留自己的账号

> 我实测过的是「局域网 IP + 明文 HTTP」这条路径；反代和 HTTPS 是上游 memos 的标准用法，我没在这台机器上验证。

---

## 四、笔记文件长什么样

每条笔记 = 一个 `data/memos/<uid>.md`，YAML front matter + 正文：

```markdown
---
uid: CdPrWRXMusbrdVP9t2zjGW
creator: owner
created: "2026-10-12T14:21:44Z"
updated: "2026-10-12T14:21:44Z"
visibility: PUBLIC
pinned: true
tags:
    - file-backed
    - memos
---

# Hello

正文就是编辑器里的原文。 #file-backed #memos
```

| 字段 | 说明 |
| --- | --- |
| `uid` | 资源名 `memos/<uid>`，同时是文件名 |
| `creator` | 作者用户名，导入时用来确定归属 |
| `created` / `updated` | RFC 3339 UTC；缺失时用文件修改时间 |
| `visibility` | `PRIVATE`(默认) / `PROTECTED` / `PUBLIC` / `SPACE`；写错值一律按 PRIVATE 处理 |
| `pinned` | 导入时生效（新建笔记时 memos 本身不允许直接置顶，所以由文件来补） |
| `tags` | 参考值：正文里的 `#标签` 优先，front matter 里的标签会合并进来 |
| `attachments` | 附件 uid 列表，字节在 `data/memos/attachments/<uid>/<原文件名>` |
| `relations` | 引用的其他笔记 uid |

没有 front matter 的纯 md 文件也合法：整个文件当正文，导入后会被补上 front matter 并规范化。

**数据目录的样子：**

```
data/
├── memos/
│   ├── CdPrWRXMusbrdVP9t2zjGG.md
│   ├── .memos-file-index.json          ← 派生索引，删了会自动重建
│   └── attachments/<uid>/<文件名>
├── assets/                             ← memos 自己的附件存储（LOCAL 模式）
└── memos_prod.db                       ← SQLite：查询引擎，不是唯一真相
```

**用 git 管理笔记：**

```bash
cd data && git init && git add -A && git commit -m "notes"
# .gitignore 默认忽略整个 data/，想跟着仓库走就把那一行删掉
```

---

## 五、配置项

全部通过环境变量，容器里在 `docker-compose.memofiles.yml` 的 `environment` 下改：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `MEMOS_MEMO_FILES` | `true`（本镜像） | 是否启用「笔记落 md 文件」。设 `false` 就是原版 memos |
| `MEMOS_PORT` | `5230` | 容器内端口 |
| `MEMOS_DATA` | `/var/opt/memos` | 数据目录，compose 把它挂到宿主机 `./data` |
| `MEMOS_DRIVER` | `sqlite` | 也可 `mysql` / `postgres`（那 md 文件就只是镜像副本） |
| `MEMOS_UID` / `MEMOS_GID` | `10001` | 笔记文件属主。设成 `id -u`/`id -g` 就不用 sudo 编辑 |
| `MEMOS_INSTANCE_URL` | 空 | 对外地址，反代后面必填 |
| `TZ` | `UTC` | 影响日志时间与 UI 显示 |
| `GOPROXY`（构建参数） | `goproxy.cn,proxy.golang.org,direct` | 构建时下载 Go 模块用；`docker build --build-arg GOPROXY=...` |

---

## 六、故障排查

这些都是我在这台 Debian 机器上真实踩到的，按现象归类。

### 容器里访问不了外网（构建卡住 / `i/o timeout`）

**现象**：`go mod download` 或 `pnpm install` 在镜像构建阶段超时，但宿主机 `curl` 同一个地址是 200。

**原因**：宿主机用本地 DNS（`/etc/resolv.conf` 指向 `127.0.0.1`），而 Docker 给容器默认塞 `8.8.8.8`，
这个网络到 8.8.8.8 不通。Docker 默认也不会把 `127.0.0.1` 写进容器的 `/etc/resolv.conf`，除非显式指定。

**处理**：

```json
// /etc/docker/daemon.json
{
  "dns": ["127.0.0.1", "223.5.5.5", "119.29.29.29"]
}
```

```bash
sudo systemctl restart docker
# 验证
docker run --rm alpine wget -q -O /dev/null -T 10 https://registry.npmjs.org/ && echo OK
```

### 构建报 `dial tcp ...:443: i/o timeout`（Go 模块）

**原因**：`proxy.golang.org` 在部分网络不可达。**处理**：镜像默认已换成 `goproxy.cn` 优先，如需自定义：

```bash
docker build --build-arg GOPROXY=https://your.proxy,direct -t memos-md .
```

### `ERR_PNPM_LOCKFILE_CONFIG_MISMATCH`

**原因**：`web/pnpm-workspace.yaml` 里声明了 `patchedDependencies` 和 `allowBuilds`，只 COPY `package.json` 和
lockfile 会让 pnpm 认为配置不匹配。**处理**：Dockerfile 已经把 `pnpm-workspace.yaml` 和 `patches/` 一起 COPY 进
去（已修，见提交历史）。

### 笔记文件属于 uid 10001，改不了

镜像以 root 启动、chown 数据目录后降到 `MEMOS_UID`/`MEMOS_GID`（默认 10001）。两种处理：

```bash
# 用 compose 覆盖文件（服务器上已经放好了 docker-compose.override.yml）
docker compose -f docker-compose.memofiles.yml -f docker-compose.override.yml up -d --force-recreate

# 或者一次性指定
MEMOS_UID=$(id -u) MEMOS_GID=$(id -g) docker compose -f docker-compose.memofiles.yml up -d --force-recreate
```

### 在服务器上改了 md，界面里没变

**这是已知限制**：外部改动在**下次启动时**导入，不是实时。因为上游 memos 没有文件监听，加一个
`fsnotify` watcher 是下一步计划。

```bash
docker restart memos-md      # 触发启动扫描
```

启动日志会写明做了什么：

```
msg="memo file storage ready" written=2 imported=1 updated=1 removed=0
```

`imported` 是新导入的文件，`updated` 是被磁盘改动回写到数据库的，`removed` 是笔记被删后一起删掉的文件。

### 刚建实例就把笔记丢进 `data/memos/`，没被导入

**已修复**：全新实例还没有账号时笔记没有归属，之前会导入失败。现在会挂起等待，**创建第一个账号的瞬间自动导入**，
日志里先出现 `memo file has no owner yet, will import it after the first account is created`。

### 上传的图片没出现在 `data/memos/attachments/`

**已修复**（提交历史里的第 3 个提交）。原因有两个：附件绑定不改变笔记文件内容，被"内容没变就跳过"挡住；以及
LOCAL 存储的字节不在数据库 row 里，而在 `reference` 指向的文件中。现在两者都处理了。

### 端口 5230 被占用

```bash
ss -ltnp | grep 5230
# 改 compose 里的端口映射： "8080:5230" 只改左边
```

### 磁盘占用

```bash
docker system df                     # 镜像/容器/缓存总量
docker builder prune                 # 清构建缓存（会变慢下次构建）
du -sh data/                         # 笔记 + 附件 + SQLite
```

---

## 七、开发与测试

```bash
# 只跑本 fork 的测试（不需要数据库容器）
go test ./core/memofile/...           # 16 个测试：格式、镜像、同步、附件、并发

# 改完代码快速验证
go vet ./core/memofile/... ./store/ ./cmd/memos/
go fmt ./core/memofile/... ./store/ ./cmd/memos/
go build ./...
```

`core/memofile` 的测试覆盖：front matter 往返、按日期写文件、store 更新/删除跟着落盘、手写文件导入、
磁盘编辑回写（含标签变化）、删库后从文件重建、附件绑定与 LOCAL 存储读取、无账号时挂起、并发重入。

> 上游自己的 `go test ./...` 在 Windows 上有两个失败：`TestLoadDeploymentConfigurationAcceptsRegularFileSymlink`
> （建符号链接需要开发者模式）和 `TestCacheEviction`（时序敏感）。我用未改动的 v0.31.0 单独验证过，同样失败，
> 与本 fork 无关。

---

## 八、路线图

按「对使用体验影响」排序，欢迎直接开 Issue：

1. **文件监听**：`fsnotify` 监听 `data/memos`，外部改动秒级同步，不用重启（当前最大短板）
2. **前端集成**：在设置页显示笔记的磁盘路径、加一个「重新扫描」按钮（现在只能重启或调 API）
3. **注释与关系**：把 `COMMENT` 关系写进 front matter，评论也能完整往返
4. **附件双向**：S3 存储的附件目前在 front matter 里只留引用，没有本地副本可以下载
5. **多用户映射**：现在导入时按 `creator` 用户名匹配，用户重命名后需要手动改文件

---

## 九、许可与署名

上游 memos 使用 MIT 许可，本 fork 沿用同一许可并保留上游的 `LICENSE` 与版权声明。发布时请：

- **保留 `LICENSE` 文件**（已在仓库里）
- 在 README 或仓库描述里写明「forked from usememos/memos」
- 不要把上游的品牌文案（tagline、logo）当成自己的宣传语——上游品牌规范在
  <https://github.com/usememos/.github/blob/main/BRAND.md>
