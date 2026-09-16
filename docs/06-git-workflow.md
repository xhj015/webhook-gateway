# Git 工作流（单人项目版）

> 文档编号：docs/06-git-workflow.md
> 定位：把"学会 git"从口号变成每周可执行的纪律。只服务一件事——让 17 周的提交历史，最后能直接变成一份可信的变更日志。

---

## 一、一次性配置（第 0 周，已完成）

| 项 | 值 |
|---|---|
| 仓库 | https://github.com/xhj015/webhook-gateway |
| 本地路径 | `D:\Golang` |
| 主分支 | `main` |
| remote | `origin` → `ssh://git@github.com/xhj015/webhook-gateway.git` |
| 身份 | `user.name=xhj015` / `user.email=1993666078@qq.com` |
| 传输 | SSH（`~/.ssh/id_ed25519`） |

**remote 为什么写成 `ssh://` 全写形式？**
本机全局配置里有一条地址改写规则 `url."git@github.com:".insteadOf = https://github.com/`（把 HTTPS 形式的 GitHub 地址统一转成 SSH，因为本机没有配 HTTPS 凭据）。用 `ssh://` 全写形式可以让 remote 本身不参与任何地址改写，最不容易出意外。**新建仓库照这个写法配。**

```bash
# 验证：下面两条都应该能列出 refs/heads/main
git ls-remote ssh://git@github.com/xhj015/webhook-gateway.git main
git ls-remote https://github.com/xhj015/webhook-gateway.git main   # 会被自动改写成 SSH
```

---

## 二、提交规范：Conventional Commits

格式：`<type>: <一句话做了什么>`

| type | 用在什么时候 |
|---|---|
| `feat` | 新增功能（接收链路、投递 worker、重试…） |
| `fix` | 修 bug |
| `docs` | 只改文档 |
| `test` | 只改测试 |
| `refactor` | 不改行为的结构调整 |
| `chore` | 脚手架、依赖、配置、`.gitignore` 之类 |

三条纪律：

1. **一次提交只做一件事**。改功能和改文档分开提交。
2. **标题写"做了什么"，不写"改了哪些文件"**。
3. **说不清为什么就在标题下空一行写正文**，写"为什么"而不是"是什么"。

示例：

```
feat: 接收 webhook 并落库为 pending 事件

先 INSERT 再返回 202，保证事务提交后才对外确认，
这样进程在返回 202 之后立刻崩溃也不会丢事件。
```

---

## 三、分支策略：主干开发，别演多人戏

单人项目只在 `main` 上开发。唯一例外是**不确定能不能成的尝试**：

```bash
git switch -c spike/db-poll-interval          # 开实验分支
# ... 试着做
git switch main
git merge --squash spike/db-poll-interval     # 成了 → 并回主干，压成一次提交
git branch -D spike/db-poll-interval          # 不成 → 直接整条丢弃
```

**整个项目里至少各练一次"合并回来"和"整条丢弃"**，否则分支这个概念永远只是纸上的。

---

## 四、里程碑 tag

| 时点 | tag 名 | 含义（能演示什么） |
|---|---|---|
| M1 结束 | `m1-go-boot` | `POST` → SQLite → 202 能跑通 |
| M2 结束 | `m2-receive` | 404 / 410 / 413 边界校验齐全 |
| M3 结束 | `m3-deliver` | 异步投递成功，Attempt 落库 |
| M4 结束 | `m4-retry` | 目标关机 → 自动重试 → 开机后送达 |
| M5 结束 | `m5-query` | 查询与手动重试跑通 |
| M6 结束 | `m6-recovery` | 投递中 kill → 重启 → 继续投递 |
| M7 结束 | `m7-test` | 状态机测试通过（`go test -race`） |
| M8 结束 | `m8-ship` | `docker run` 一条命令跑起来 |
| W17 | `v0.1.0` | 正式发布（annotated tag） |

```bash
git tag m3-deliver                                    # 里程碑用轻量 tag
git tag -a v0.1.0 -m "V0.1.0: 接收 → 落库 → 投递 → 重试 → 查询 闭环完成"
git push origin --tags
```

**铁律：tag 绑定"能演示"，不是"代码写完"。** 演示不出来的那一周，就不打 tag。

---

## 五、每周固定动作（30 分钟）

对应执行计划里的"提交"时段：

1. `git status` 看一眼改了哪些文件——有没有混进 `*.db`、`.env`、临时脚本
2. 分批 `git add`（`git add -p` 逐块挑更好），**不要无脑 `git add .`**
3. 提交，标题按第二节写
4. 顺手更新设计文档或 README（有变化才写）
5. 里程碑到了就 `git tag`
6. `git push`，然后刷新 GitHub 页面确认真的上去了

> "命令返回成功"和"远端真的有了"是两件事，尤其是第一次用新 remote 的时候。

---

## 六、发布流程（第 17 周）

```bash
git tag -a v0.1.0 -m "V0.1.0 ..."
git push origin main --tags
# 然后到 GitHub 上基于这个 tag 建 Release，把 CHANGELOG 的 V0.1.0 段落贴进去
```

`CHANGELOG.md` 不用手写——提交历史本身就是变更日志：

```bash
git log --oneline --no-merges v0.1.0
```

按 `feat` / `fix` / 其他三组归一下类，就是一份像样的 CHANGELOG。

---

## 七、失误处理：五个命令的分工

| 情况 | 命令 | 结果 |
|---|---|---|
| 提交已经推到远端，要撤销 | `git revert <sha>` | 新增一个反向提交，历史保留，最安全 |
| 只改了工作区，没 add | `git restore <file>` | 丢弃工作区改动 |
| 已 add，没 commit | `git restore --staged <file>` | 退回未暂存 |
| 提交了但没推，想撤销提交保留改动 | `git reset --soft HEAD~1` | 改动回到暂存区 |
| 想临时放下手头的活 | `git stash` / `git stash pop` | 暂存现场、稍后恢复 |

**已经推送过的历史一律用 `revert`，不用 `reset --hard`。**

### 加分练习（执行计划里点名的）

故意留一次真实失误在远端，然后用 `git revert` 把它回滚并推上去。亲手走一遍，比背 20 个命令管用。另外争取至少经历一次合并冲突——开 `spike/` 分支碰一次就有机会。

---

## 八、Windows 相关的两个坑

1. **CRLF**：仓库根目录已放 `.gitattributes`（`* text=auto eol=lf`）。没有它，每次 `git add` 都会刷 `LF will be replaced by CRLF` 警告，而且跨机器 diff 会整篇失真。
2. **不该进仓库的东西**：`.gitignore` 已在第 0 周写好——`*.exe`、`bin/`、`*.db*`、`.env`。**SQLite 数据库文件和密钥永远不进仓库。**

---

## 九、这份文档什么时候更新

- 新增了分支策略或发布流程 → 补进来
- 踩到新的 git 坑 → 补进第八节
- 其他情况不要动它
