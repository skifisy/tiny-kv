# TinyKV Project2A 实现说明

本文档记录我在 `project2a`（`2AA + 2AB + 2AC`）中的实现思路、核心改动点与测试验证过程。

## 1. 实现目标

Project2A 的目标分三块：

- **2AA：Leader Election**
  - 选举超时与心跳超时驱动
  - Follower/Candidate/Leader 状态切换
  - RequestVote / Heartbeat 等消息处理
- **2AB：Log Replication**
  - Leader 追加日志并向 Follower 同步
  - Follower 按 prevLogIndex/prevLogTerm 校验并处理冲突
  - Leader 根据多数派推进 `commit`
- **2AC：RawNode 接口**
  - 通过 `Ready` 向上层暴露待持久化、待应用、待发送的数据
  - `HasReady` 判断是否有增量状态
  - `Advance` 回收已处理进度

---

## 2. 主要改动文件

- `raft/log.go`
- `raft/raft.go`
- `raft/rawnode.go`

没有改动 raftstore / kv 层，范围仅限 2A。

---

## 3. `raft/log.go` 实现要点

### 3.1 `newLog(storage Storage)`

实现了从 `Storage` 恢复日志：

- 读取 `FirstIndex` / `LastIndex`
- 拉取 `[first, last+1)` entries
- 读取 `first-1` 的 term 作为 dummy entry
- 初始化：
  - `committed = first - 1`
  - `applied = first - 1`
  - `stabled = last`

这样保证 RaftLog 的内存结构和存储一致。

### 3.2 基础访问函数

实现了：

- `allEntries()`：返回非 dummy entries
- `unstableEntries()`：返回 `stabled` 之后的日志
- `nextEnts()`：返回 `applied < index <= committed` 的日志
- `LastIndex()`：最后一条日志索引
- `Term(i)`：优先读内存，必要时回退到 storage

### 3.3 一个关键细节

`unstableEntries()` 在“没有不稳定日志”时返回**空切片**而不是 `nil`，用于兼容 `TestRawNodeRestart2AC` 的严格比较（`[]` 与 `nil` 在 `reflect.DeepEqual` 下不同）。

---

## 4. `raft/raft.go` 实现要点

## 4.1 `newRaft`

完成节点初始化：

- 从 `Storage.InitialState()` 恢复 `HardState` 和 `ConfState`
- 创建 `RaftLog`
- 初始化 `Prs`（每个 peer 的 `Match/Next`）
- 同步 `Term` / `Vote` / `committed` / `applied`
- 初始化随机选举超时

### 4.2 发送 RPC

- `sendAppend(to)`
  - 以 `pr.Next-1` 作为 prevLog
  - 组装 AppendEntries（包含 entries + commit）
- `sendHeartbeat(to)`
  - 发送 heartbeat
  - commit 字段按 follower 进度截断：`min(leaderCommitted, pr.Match)`
  - 避免把 follower 未拥有的日志误告为可提交

### 4.3 计时器 `tick()`

- Leader 走 heartbeat 计时，到点触发 `MsgBeat`
- Follower/Candidate 走 election 计时，到点触发 `MsgHup`

### 4.4 状态切换

- `becomeFollower(term, lead)`
- `becomeCandidate()`
- `becomeLeader()`

`becomeLeader()` 会：

- 初始化所有 peer 的 progress
- 追加 noop entry（题目要求）
- 单节点场景直接提交
- 多节点广播 append

### 4.5 `Step(m)` 核心逻辑

实现了统一入口：

1. **term 处理**
   - 对网络消息处理 term 前后关系
   - 本地消息（`MsgHup/MsgBeat/MsgPropose`）不走 term 拦截
2. **通用消息处理**
   - `MsgHup`：发起选举
   - `MsgRequestVote`：日志新旧比较 + 投票
   - `MsgAppend`：交给 `handleAppendEntries`
   - `MsgHeartbeat`：交给 `handleHeartbeat`
3. **按角色处理**
   - Candidate 统计投票，达到多数成为 Leader
   - Leader 处理 Propose / AppendResponse / HeartbeatResponse

### 4.6 `handleAppendEntries`

实现了论文中的核心规则：

- prevLog 不匹配则拒绝
- 冲突日志（同 index 不同 term）截断并覆盖
- 追加新日志
- 提交推进使用

$$commit = \min(leaderCommit, lastNewIndex)$$

这里 `lastNewIndex = m.Index + len(m.Entries)`，而不是本地 `LastIndex()`，避免错误超前提交。

### 4.7 `handleHeartbeat`

- 刷新 follower 的 election 计时
- 按 `min(m.Commit, localLastIndex)` 推进提交
- 回 `MsgHeartbeatResponse`

---

## 5. `raft/rawnode.go` 实现要点

### 5.1 增量状态缓存

在 `RawNode` 中维护：

- `prevSoftState`
- `prevHardState`

用于判断是否产生新的 `Ready`。

### 5.2 `NewRawNode`

- 创建 `Raft`
- 初始化 `prevSoftState/prevHardState`

### 5.3 `Ready()`

判断当前是否有“可以交给上层处理”的新状态
- 有新的待发送消息
- 有新的日志要持久化
- 有新的 committed entries 要应用
- 有新的 snapshot 要处理


组装并返回：

- `SoftState`（有变化才返回）
- `HardState`（有变化才返回）
- `Entries`（unstable）
- `CommittedEntries`
- `Messages`
- `Snapshot`（2A阶段基本为空）

并清空 `r.msgs`，防止重复发送。

### 5.4 `HasReady()`

- 取出当前这批“准备好”的数据。
- 返回一个 Ready 结构，里面通常包含：
- Entries：需要写盘的日志
  - CommittedEntries：已经提交、可以应用到状态机的日志
  - Messages：需要发送给其他节点的消息
  - Snapshot：快照数据
  - SoftState / HardState：状态变化


任一条件为真即返回 true：

- 有待发送消息
- 有 unstable entries
- 有 committed but not applied entries
- snapshot 待处理
- soft/hard state 与上次不同

### 5.5 `Advance(rd)`

- 根据 `rd.Entries` 推进 `stabled`
- 根据 `rd.CommittedEntries` 推进 `applied`
- 更新 `prevSoftState/prevHardState`

---

## 6. 调试过程中的关键问题

### 问题 1：2AA 中不发心跳、不触发选举

根因：本地消息没带 term，被当成旧 term 消息提前丢弃。

修复：`Step` 中将 `MsgHup/MsgBeat/MsgPropose` 从 term 拦截逻辑中排除。

### 问题 2：2AB 中 follower commit 超前

根因：`handleAppendEntries` 使用了 `min(leaderCommit, localLastIndex)`，在“没有新日志”的 append 下会错误推进。

修复：改为 `min(leaderCommit, lastNewIndex)`。

### 问题 3：2AB 心跳导致落后节点错误提交

根因：heartbeat 直接携带 leader 的全局 committed。

修复：发给每个 follower 的 heartbeat commit 使用 `min(leaderCommitted, followerMatch)`。

### 问题 4：2AC `Ready` 比较失败

根因：测试期望 `Entries: []`，实现返回 `nil`。

修复：无 unstable entries 时返回空切片。

---

## 7. 验证结果

执行命令：

- `make project2a`

最终结果：

- `2AA` 全部通过
- `2AB` 全部通过
- `2AC` 全部通过
- `PASS`（`./raft -run 2A`）

---

## 8. 后续建议

进入 2B 前建议先保留当前状态并继续按测试驱动推进：

1. 先实现 `PeerStorage.SaveReadyState`
2. 再完成 `proposeRaftCommand` 与 `HandleRaftReady`
3. 全程用 `make project2b` 回归验证

这样可以保持改动面可控，定位问题更快。
