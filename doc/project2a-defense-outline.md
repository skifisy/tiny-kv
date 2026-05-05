# TinyKV Project2A 答辩口述提纲（精简版）

> 目标：用于 5–8 分钟口述汇报，强调“做了什么、为什么这样做、如何验证”。

## 0. 开场（20 秒）

我完成的是 TinyKV Project2A，对应三部分：

- 2AA：Leader Election
- 2AB：Log Replication
- 2AC：RawNode Interface

整体改动只在 raft 核心层，主要文件是 `raft/log.go`、`raft/raft.go`、`raft/rawnode.go`。

---

## 1. 我先解决了什么问题（约 1 分钟）

### 1.1 2AA：节点必须能稳定选主

我实现了三件事：

1. **tick 驱动逻辑时钟**：
   - Leader 到心跳超时发 `MsgBeat`
   - Follower/Candidate 到选举超时发 `MsgHup`
2. **角色切换函数**：
   - `becomeFollower`
   - `becomeCandidate`
   - `becomeLeader`
3. **选举消息处理**：
   - `MsgRequestVote` / `MsgRequestVoteResponse`
   - 候选人统计多数票成为 Leader

另外，选举超时做了随机化，减少分票冲突；新 Leader 会追加 noop entry，满足测试假设。

### 1.2 2AB：日志复制与提交推进

我补齐了完整链路：

- Leader 收到 `MsgPropose` 后本地 append，并广播 `MsgAppend`
- Follower 在 `handleAppendEntries` 里做 prevLog 校验、冲突截断、追加新日志
- Leader 在 `MsgAppendResponse` 中根据多数派推进 `committed`

提交推进遵循关键规则：

$$commit = \min(leaderCommit, lastNewIndex)$$

并且 Leader 只对“当前任期日志”按多数派推进提交，符合 Raft 安全性要求。

### 1.3 2AC：把 Raft 内部状态正确暴露给上层

我实现了 `RawNode` 的三个关键接口：

- `Ready()`：返回
  - 待持久化 `Entries`
  - 待应用 `CommittedEntries`
  - 待发送 `Messages`
  - Soft/Hard state 增量
- `HasReady()`：判断是否有新工作
- `Advance()`：推进 `stabled/applied`，避免重复返回

---

## 2. 关键设计点（老师常问，约 2 分钟）

### 2.1 为什么本地消息不参与 term 旧新判断？

`MsgHup`、`MsgBeat`、`MsgPropose` 是本地触发消息，测试里通常不带 term。
如果按网络消息统一做 `m.Term < r.Term` 拦截，会把这些消息错误丢弃，导致“不发心跳、不发起选举”。
所以我将本地消息从 term 拦截逻辑中排除。

### 2.2 为什么 heartbeat 的 commit 不能直接发 leader 的 committed？

因为某些 follower 可能还没追上日志。
如果直接发全局 committed，会让落后节点错误前推提交。
我按 follower 进度截断：

$$hbCommit = \min(leaderCommitted, followerMatch)$$

这样 follower 只会提交自己已匹配的日志范围。

### 2.3 为什么处理 append 时用 `lastNewIndex` 而不是 `localLastIndex`？

Raft 语义是“本次 RPC 能证明的提交上限”，不是“本地当前日志上限”。
用 `localLastIndex` 可能在“本次没有携带新日志”场景下超前提交。
所以我用：

$$commit = \min(leaderCommit, m.Index + len(m.Entries))$$

### 2.4 为什么要区分空切片和 nil？

`rawnode_test` 中存在 `reflect.DeepEqual` 精确比较，`[]` 与 `nil` 不相等。
因此在无不稳定日志时返回空切片，保证 Ready 结构和测试期望完全一致。

---

## 3. 实现路径（1 分钟）

我是按“测试驱动 + 分层收敛”做的：

1. 先补 `log.go` 基础能力（索引、term、unstable、nextEnts）
2. 再补 `raft.go` 主状态机（选举、复制、提交）
3. 最后补 `rawnode.go` Ready/Advance 闭环
4. 每次改动后直接跑 `make project2a`，根据失败用例定点修复

---

## 4. 结果与验证（30 秒）

最终 `make project2a` 全部通过：

- 2AA 全通过
- 2AB 全通过
- 2AC 全通过

说明基础 Raft 核心行为和上层接口契约已经闭环。

---

## 5. 若继续做 2B，我的推进策略（30 秒）

下一步我会按以下顺序推进：

1. 先实现 `PeerStorage.SaveReadyState`
2. 再完成 `proposeRaftCommand`
3. 最后补 `HandleRaftReady` 的持久化/应用/回调流程

每步都用 `make project2b` 回归，避免一次改动过大导致定位困难。

---

## 6. 可直接背诵的 20 秒总结

Project2A 我完成了 Raft 的选举、日志复制和 RawNode 接口闭环。核心上我重点保证了三件事：本地消息与网络消息分流处理、append/heartbeat 的提交边界正确、Ready/Advance 状态机不重复不丢失。最终 `make project2a` 全绿，说明实现满足课程测试要求并具备进入 2B 的基础。
