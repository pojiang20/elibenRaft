package raft

import (
	"sync"
	"time"
)

type CMState int

const (
	Follower CMState = iota
	Candidate
	Leader
	Dead
)

type ConsensusModule struct {
	//身份
	id int
	//当前状态
	state CMState
	//当前任期
	term int
	//活跃时间
	aliveTime int64
	//其余节点
	peersId []int
	//server服务
	server *Server
	//锁服务
	lock sync.Mutex
}

// 节点初始化，选举计时器
func NewConsensusModule(id int,peers []int,server *Server) *ConsensusModule {
	cm := &ConsensusModule{
		id: id,
		peersId: peers,
		state: Follower,
		server: server,
	}
	go cm.runElectionTimer()
	return cm
}

// Report reports the state of this CM.
func (cm *ConsensusModule) Report() (id int, term int, isLeader bool) {
	cm.lock.Lock()
	defer cm.lock.Unlock()
	return cm.id, cm.term, cm.state == Leader
}

func (cm ConsensusModule) Stop() {
	cm.lock.Lock()
	cm.state=Dead
	cm.lock.Unlock()
}

/*
CM的选举触发器
身份：Candidate\Follower
任期：不变
触发条件：超时
*/
func (cm ConsensusModule) runElectionTimer() {
	cm.lock.Lock()
	savedTerm := cm.term
	cm.lock.Unlock()

	ticker:=time.NewTicker()
	defer ticker.Stop()
	for {
		<-ticker.C

		cm.lock.Lock()
		if cm.state != Candidate && cm.state != Follower {
			cm.lock.Unlock()
			return
		}
		if cm.term != savedTerm {
			//通常任期不对会导致身份变换，但这里没有
			cm.lock.Unlock()
			return
		}
		//超时则触发选举
		if (time.Now().UnixMilli()-cm.aliveTime) > {
			cm.startElection()
			cm.lock.Unlock()
			return
		}
		cm.lock.Unlock()
	}
}

/*
发起选举，与其他节点通信获取majority投票
身份：Candidate
任期：+1
*/
func (cm ConsensusModule) startElection(){
	cm.state=Candidate
	//Tip: 更新任期是发生在发起选举
	cm.term += 1
	//给自己投票数量
	getVotesNum := 1
	totalVotesNum := len(cm.peersId)

	args := AppendEntriesArgs{}
	reply := AppendEntriesReply{}

	for _,peerId := range cm.peersId{
		go func(peerId int) {
			if err:=cm.server.Call(peerId,"",args,&reply); err==nil{
				//身份检查
				if cm.state != Candidate{
					return
				}
				//任期检查
				if reply.term>cm.term{
					cm.becomeFollower()
				}
				if reply.term==cm.term{
					if reply.voteGranted{
						getVotesNum+=1
						//超过大多数
						if getVotesNum*2 > totalVotesNum + 1 {
							//当选leader
							cm.becomeLeader()
						}
					}
				}
			}
		}(peerId)
	}
}

func (cm ConsensusModule) becomeFollower(){
	cm.lock.Lock()
	cm.state = Follower
	cm.lock.Unlock()
}

//定期向其他节点发起心跳
func (cm ConsensusModule) becomeLeader(){
	cm.state=Leader
	//更新任期
	cm.term ++
	ticker := time.NewTicker()
	for{
		<-ticker.C
		cm.sendHeartbeat()
	}
}

func (cm ConsensusModule) sendHeartbeat(){
	args := AppendEntriesArgs{}
	reply := AppendEntriesReply{}
	for _,peerId := range cm.peersId {
		if err:=cm.server.Call(peerId,"",args,&reply); err==nil{
		}
	}
}

type AppendEntriesArgs struct {
	term int
	requestCandidateId int
}

//Tips: 并不关心投给谁
type AppendEntriesReply struct {
	//响应时的任期
	term int
	//是否已经投票
	voteGranted bool
}

