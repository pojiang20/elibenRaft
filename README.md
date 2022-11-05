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