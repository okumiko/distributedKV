package raft

//使用前上锁
func (rf *Raft) StartElection() {

	rf.currentTerm += 1 //新一轮选举将currentTerm加一
	rf.votedFor = rf.me //投给自己
	rf.persist()        //持久化

	grantedVotes := 1               //投票统计,默认投给自己
	args := rf.genRequestVoteArgs() //请求投票rpc args

	DPrintf("<Node %v> starts election with RequestVoteArgs %v", rf.me, args)

	for peer := range rf.peers {
		if peer == rf.me {
			continue
		}
		//并行异步请求投票
		go func(peer int) {
			reply := new(RequestVoteReply)
			if rf.sendRequestVote(peer, args, reply) { //rpc调用成功返回
				rf.mu.Lock()
				defer rf.mu.Unlock()

				//有锁，并不是并发执行的，不会有race
				rf.handleRequestVoteReply(peer, grantedVotes, args, reply)
			}
		}(peer)
	}
}

func (rf *Raft) genRequestVoteArgs() *RequestVoteArgs {
	return &RequestVoteArgs{
		Term:         rf.currentTerm,
		CandidateId:  rf.me,
		LastLogTerm:  rf.getLastLog().Term,
		LastLogIndex: rf.getLastLog().Index,
	}
}

type RequestVoteArgs struct {
	Term         int //candidate的任期号
	CandidateId  int //发起投票的candidate的ID
	LastLogTerm  int //candidate的最高日志条目索引
	LastLogIndex int //candidate的最高日志条目的任期号
}

type RequestVoteReply struct {
	Term        int  //服务器的当前任期号，让candidate更新自己
	VoteGranted bool //如果是true，意味着candidate收到了选票
}

func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}

//处理请求投票handle
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	defer rf.persist() //持久化
	defer DPrintf("{Node %v}'s state is {state %v,term %v,commitIndex %v,lastApplied %v,firstLog %v,lastLog %v} before processing requestVoteArgs %v and reply requestVoteReply %v", rf.me, rf.state, rf.currentTerm, rf.commitIndex, rf.lastApplied, rf.getFirstLog(), rf.getLastLog(), args, reply)

	//识别出过期的server，更新它的term
	//1.term小；2.同一个任期接到了两个投票请求，voteFor说明已经投了一个票，如果投的不是之前投的就拒绝投票(一个term只能投一次票，要不然票数都乱了)
	if args.Term < rf.currentTerm || (args.Term == rf.currentTerm && rf.votedFor != -1 && rf.votedFor != args.CandidateId) {
		reply.Term, reply.VoteGranted = rf.currentTerm, false
		DPrintf("here1")
		return
	}
	//投票，变成跟随者
	if args.Term > rf.currentTerm {
		rf.ToState(StateFollower)  //当前节点改变状态
		rf.currentTerm = args.Term //更新当前节点任期
	}
	if !rf.isLogUpToDate(args.LastLogTerm, args.LastLogIndex) {
		reply.Term, reply.VoteGranted = rf.currentTerm, false
		DPrintf("here2")
		return
	}
	rf.votedFor = args.CandidateId
	resetTimer(rf.electionTimer, RandomizedElectionTimeout()) //reset选举定时器
	reply.Term, reply.VoteGranted = rf.currentTerm, true
}

//使用前加锁
func (rf *Raft) handleRequestVoteReply(peer int, grantedVotes int, args *RequestVoteArgs, reply *RequestVoteReply) {
	DPrintf("<Node %v> receives RequestVoteReply %v from <Node %v> after sending RequestVoteArgs %v in term %v", rf.me, reply, peer, args, rf.currentTerm)

	//用rf.currentTerm == request.Term跳过过期的请求回复
	if rf.currentTerm == args.Term && rf.state == StateCandidate {
		if reply.VoteGranted { //回复同意投票
			grantedVotes += 1                   //计数加一
			if grantedVotes > len(rf.peers)/2 { //超过半数以上
				rf.ToState(StateLeader) //本节点变为领导者

				DPrintf("<Node %v> receives majority votes in term %v", rf.me, rf.currentTerm)
			}
		} else if reply.Term > rf.currentTerm { //回复不同意，并且收到的回复任期大于当前节点任期
			DPrintf("<Node %v> finds a new leader <Node %v> with term %v and steps down in term %v", rf.me, peer, reply.Term, rf.currentTerm)
			rf.currentTerm = reply.Term //更新本节点任期
			rf.ToState(StateFollower)   //本节点变为跟随者
			rf.persist()                //持久化
		}
	}
}

//用于投票时，投票者判断candidate的日志是否至少和接收者的日志一样新(up-to-date)
func (rf *Raft) isLogUpToDate(LastLogTerm, LastLogIndex int) bool {
	localLastLog := rf.getLastLog()
	if LastLogTerm != localLastLog.Term {
		//任期不同，任期优先级最高
		return LastLogTerm > localLastLog.Term
	} else {
		//至少一样新
		return LastLogIndex >= localLastLog.Index
	}
}

func (rf *Raft) replicateOneRound(peer int) {
	rf.mu.RLock()
	if rf.state != StateLeader {
		rf.mu.RUnlock()
		return
	}
	prevLogIndex := rf.nextIndex[peer] - 1
	if prevLogIndex < rf.getFirstLog().Index { //preLogIndex比本地firstLog还小，说明无法通过本地日志恢复，只能用快照
		// only snapshot can catch up
		args := &InstallSnapshotArgs{
			Term:              rf.currentTerm,
			LeaderId:          rf.me,
			LastIncludedIndex: rf.logs[0].Index,
			LastIncludedTerm:  rf.logs[0].Term,
			Data:              rf.persister.ReadSnapshot(),
		}
		rf.mu.RUnlock()
		response := new(InstallSnapshotReply)
		//发送快照
		if rf.sendInstallSnapshot(peer, args, response) {
			rf.mu.Lock()
			rf.handleInstallSnapshotResponse(peer, args, response)
			rf.mu.Unlock()
		}
	} else { //preLogIndex可以发送
		// just entries can catch up
		request := rf.genAppendEntriesArgs(prevLogIndex)
		rf.mu.RUnlock()
		response := new(AppendEntriesReply)
		if rf.sendAppendEntries(peer, request, response) {
			rf.mu.Lock()
			rf.handleAppendEntriesResponse(peer, request, response)
			rf.mu.Unlock()
		}
	}
}
