# elibenRaft
learn raft from eliben raft tutorial

https://eli.thegreenplace.net/2020/implementing-raft-part-0-introduction/

2022-08-28 12:22:56 part1完，没什么收获，先搁置。

2022-09-14 22:06:40 进行到part3优化部分暂停 


2022-11-04 18:19:09 part1重写

### 笔记
#### 如果一个节点离线，一段时间后再上线会发生什么？
离线期间该节点会不断地更新`term`且发起投票，导致`term`比上线的稳定集群中任期大很多。
该节点上线后，发起选举，其他节点接受到选举请求，由于任期更大导致所有节点变成`follower`，但又因为任期不同并不会投票给该节点。也就是该节点的上线不会因为该节点任期大成为`leader`，而是会导致一个无主的状态出现。

#### 提供服务
raft集群提供服务的方式是：选出的Leader为客户端请求提供服务。它有如下特点：

- 操作以日志的形式记录下来，并且从Leader单向传播到Follower
- 客户端如何找到Leader有下面三种情况，结论就是客户端多发几次请求，一定能放到问集群中的Leader。
    1. 访问到的节点就是Leader
    2. 访问到Follower，Follower通过心跳知道LeaderID，并告知客户端。
    3. 访问节点宕机得不到响应，客户端重新随机发送请求即可。
#### 日志信息
一条日志一定包含三个信息：操作、任期、日志号。
#### 日志同步和提交
这里采用2PC（两阶段提交）策略，先Leader保证日志复制到了大多数节点（Propose阶段），再提交日志（Commit阶段）。如果Propose阶段失败，就会在Commit阶段放弃Propose操作。
> 2PC的问题在于，voter在Propose阶段统一之后，后续没有收到消息，它无法区分是Propose阶段没有通过，还是Commit阶段节点宕机导致无法发送消息。[参考](https://zhuanlan.zhihu.com/p/35298019)
> 所以引入3PC（三阶段提交），在Propose和Commit阶段之间添加了PreCommit，即PreCommit阶段节点接收到消息则意味着Propose阶段已经全部通过，因此节点可以区分Propose未通过和Commit宕机两种情况。但是这里仍然会出现PreCommit阶段无法区分Propose未通过还是PreCommit通信问题这两种情况。

#### AppendEntryRpc
追加日志条目信息是由Leader单向流向Follower的信息，用于Propose阶段的Majority日志统一。追加日志条目RPC有两个阶段：先定位主从之间最晚相同日志位置（**一致性检查**）+将定位后的日志条目与Leader统一。这样既保证了顺序又保证了内容，Propose同步阶段有下面三种可能：

1. Follower没响应，Leader不断重发。
2. Follower崩溃后恢复，追加条目RPC先**一致性检查**进行定位，再进行同步。
> AppendEntryRpc会携带前一个日志条目的位置（任期+日志号确定唯一位置），Follower接收到这条信息与自己的日志进行匹配，如果相同位置内容不同则拒绝日志，这样Leader发送的AppendEntryRpc会发送再向前一个日志条目的位置，重复操作，知道Follower找到匹配的日志不拒绝为止。

3. Leader崩溃，如果Leader不崩溃则Propose阶段通常Leader的日志数要领先于Follower，所以由于Leader崩溃后由上线，就会出现新Leader的日志落后/冲突于Follower的情况（前任Leader）。
    - 这种情况存在且不影响，因为这都是Propose阶段，只有Commit阶段才生效。
    - Raft处理这种情况和追加Follower没有区别，都是一致性检查+同步，直接把冲突/领先的内容覆盖。

这种机制的结果就是：Leader只需要不断发送AppendEntriesRPC，就可以完成一致性检查和追加同步的过程，最后Majority趋于一致之后，才进入Commit阶段
[参考](https://www.bilibili.com/video/BV1VY4y1e7px/?spm_id_from=333.788&vd_source=1909dad7cca671c67a4883b99993d9f2)
#### nextIndex更新
前提：①任期和编号唯一确定一个日志。②两个节点的日志相同，则前面所有的日志都相同。
主尝试同步日志：查看主与从的最新的相同日志，并将后面覆盖。查看主从最新相同日志的步骤，就是一致性检测。
以主从一对一的角度来看，主从一致性检测的过程就是不断的appendEntry，然后返回false/true的过程。具体过程是主当选之后，将与从的
以主从一对一的角度来看，一致性检测就是确定两者相同日志Index的过程。具体如下：

- 主当选后，将Index初始为自己最后的一个日志坐标，在发送心跳时发送Index给从。
- 从接收心跳，比较主的Index，如果相同就返回true，如果不同就返回false。
- 主接收响应，如果响应为true则表示Index是相同的，则将Index之后的所有日志都发送给从来同步日志。如果响应为false则表示Index是不同的，主会回退Index。（回退简单的策略是Index--）
#### appendEntriy
appendEntry是Leader对Follower发起的，F收到请求，会根据args的值来进行判断。根据`LastSameLogIndex`来进行一致性检查，日志不同则返回false表示一致性检查不通过，如果一致性检查通过就追加日志。