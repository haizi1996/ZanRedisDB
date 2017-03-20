package datanode_coord

import (
	"bytes"
	"encoding/json"
	"errors"
	. "github.com/absolute8511/ZanRedisDB/cluster"
	"github.com/absolute8511/ZanRedisDB/common"
	node "github.com/absolute8511/ZanRedisDB/node"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	MaxRetryWait         = time.Second * 3
	ErrNamespaceNotReady = NewCoordErr("namespace node is not ready", CoordLocalErr)
)

const (
	MAX_RAFT_JOIN_RUNNING = 5
)

func GetNamespacePartitionFileName(namespace string, partition int, suffix string) string {
	var tmpbuf bytes.Buffer
	tmpbuf.WriteString(namespace)
	tmpbuf.WriteString("-")
	tmpbuf.WriteString(strconv.Itoa(partition))
	tmpbuf.WriteString(suffix)
	return tmpbuf.String()
}

func GetNamespacePartitionBasePath(rootPath string, namespace string, partition int) string {
	return filepath.Join(rootPath, namespace)
}

type DataCoordinator struct {
	clusterKey       string
	register         DataNodeRegister
	pdMutex          sync.Mutex
	pdLeader         NodeInfo
	myNode           NodeInfo
	stopChan         chan struct{}
	tryCheckUnsynced chan bool
	wg               sync.WaitGroup
	stopping         int32
	catchupRunning   int32
	localNSMgr       *node.NamespaceMgr
}

func NewDataCoordinator(cluster string, nodeInfo *NodeInfo, nsMgr *node.NamespaceMgr) *DataCoordinator {
	coord := &DataCoordinator{
		clusterKey:       cluster,
		register:         nil,
		myNode:           *nodeInfo,
		stopChan:         make(chan struct{}),
		tryCheckUnsynced: make(chan bool, 1),
		localNSMgr:       nsMgr,
	}

	return coord
}

func (self *DataCoordinator) GetMyID() string {
	return self.myNode.GetID()
}

func (self *DataCoordinator) GetMyRegID() uint64 {
	return self.myNode.RegID
}

func (self *DataCoordinator) SetRegister(l DataNodeRegister) error {
	self.register = l
	if self.register != nil {
		self.register.InitClusterID(self.clusterKey)
		if self.myNode.RegID <= 0 {
			var err error
			self.myNode.RegID, err = self.register.NewRegisterNodeID()
			if err != nil {
				CoordLog().Errorf("failed to init node register id: %v", err)
				return err
			}
			err = self.localNSMgr.SaveMachineRegID(self.myNode.RegID)
			if err != nil {
				CoordLog().Errorf("failed to save register id: %v", err)
				return err
			}
		}
		self.myNode.ID = GenNodeID(&self.myNode, "datanode")
		CoordLog().Infof("node start with register id: %v", self.myNode.RegID)
	}
	return nil
}

func (self *DataCoordinator) Start() error {
	if self.register != nil {
		if self.myNode.RegID <= 0 {
			CoordLog().Errorf("invalid register id: %v", self.myNode.RegID)
			return errors.New("invalid register id for data node")
		}
		err := self.register.Register(&self.myNode)
		if err != nil {
			CoordLog().Warningf("failed to register coordinator: %v", err)
			return err
		}
	}
	if self.localNSMgr != nil {
		self.localNSMgr.Start()
	}
	self.wg.Add(1)
	go self.watchPD()

	err := self.loadLocalNamespaceData()
	if err != nil {
		close(self.stopChan)
		return err
	}

	self.wg.Add(1)
	go self.checkForUnsyncedNamespaces()
	return nil
}

func (self *DataCoordinator) Stop() {
	if atomic.LoadInt32(&self.stopping) == 1 {
		return
	}
	self.prepareLeavingCluster()
	close(self.stopChan)
	self.wg.Wait()
}

func (self *DataCoordinator) GetCurrentPD() NodeInfo {
	self.pdMutex.Lock()
	defer self.pdMutex.Unlock()
	return self.pdLeader
}

func (self *DataCoordinator) GetAllPDNodes() ([]NodeInfo, error) {
	return self.register.GetAllPDNodes()
}

func (self *DataCoordinator) watchPD() {
	defer self.wg.Done()
	leaderChan := make(chan *NodeInfo, 1)
	if self.register != nil {
		go self.register.WatchPDLeader(leaderChan, self.stopChan)
	} else {
		return
	}
	for {
		select {
		case n, ok := <-leaderChan:
			if !ok {
				return
			}
			self.pdMutex.Lock()
			if n.GetID() != self.pdLeader.GetID() ||
				n.Epoch != self.pdLeader.Epoch {
				CoordLog().Infof("pd leader changed: %v", n)
				self.pdLeader = *n
			}
			self.pdMutex.Unlock()
		}
	}
}

func (self *DataCoordinator) checkLocalNamespaceMagicCode(nsInfo *PartitionMetaInfo, tryFix bool) error {
	if nsInfo.MagicCode <= 0 {
		return nil
	}
	err := self.localNSMgr.CheckMagicCode(nsInfo.GetDesp(), nsInfo.MagicCode, tryFix)
	if err != nil {
		CoordLog().Infof("namespace %v check magic code error: %v", nsInfo.GetDesp(), err)
		return err
	}
	return nil
}

type PartitionList []PartitionMetaInfo

func (self PartitionList) Len() int { return len(self) }
func (self PartitionList) Less(i, j int) bool {
	return self[i].Partition < self[j].Partition
}
func (self PartitionList) Swap(i, j int) {
	self[i], self[j] = self[j], self[i]
}

func (self *DataCoordinator) loadLocalNamespaceData() error {
	if self.localNSMgr == nil {
		return nil
	}
	namespaceMap, _, err := self.register.GetAllNamespaces()
	if err != nil {
		if err == ErrKeyNotFound {
			return nil
		}
		return err
	}
	sortedParts := make(PartitionList, 0)
	for namespaceName, namespaceParts := range namespaceMap {
		sortedParts = sortedParts[:0]
		for _, part := range namespaceParts {
			sortedParts = append(sortedParts, part)
		}
		sort.Sort(sortedParts)
		for _, nsInfo := range sortedParts {
			localNamespace := self.localNSMgr.GetNamespaceNode(nsInfo.GetDesp())
			shouldLoad := self.isNamespaceShouldStart(nsInfo)
			if !shouldLoad {
				if len(nsInfo.GetISR()) >= nsInfo.Replica && localNamespace != nil {
					self.removeLocalNamespaceFromRaft(localNamespace, true)
				}
				continue
			}
			if localNamespace != nil {
				// already loaded
				joinErr := self.ensureJoinNamespaceGroup(nsInfo, localNamespace)
				if joinErr != nil && joinErr != ErrNamespaceConfInvalid {
					// we ensure join group as order for partitions
					break
				}
				continue
			}
			CoordLog().Infof("loading namespace: %v", nsInfo.GetDesp())
			if namespaceName == "" {
				continue
			}
			checkErr := self.checkLocalNamespaceMagicCode(&nsInfo, true)
			if checkErr != nil {
				CoordLog().Errorf("failed to check namespace :%v, err:%v", nsInfo.GetDesp(), checkErr)
				continue
			}

			localNamespace, coordErr := self.updateLocalNamespace(&nsInfo)
			if coordErr != nil {
				CoordLog().Errorf("failed to init/update local namespace %v: %v", nsInfo.GetDesp(), coordErr)
				continue
			}

			dyConf := &node.NamespaceDynamicConf{}
			localNamespace.SetDynamicInfo(*dyConf)
			localErr := self.checkAndFixLocalNamespaceData(&nsInfo, localNamespace)
			if localErr != nil {
				CoordLog().Errorf("check local namespace %v data need to be fixed:%v", nsInfo.GetDesp(), localErr)
				localNamespace.SetDataFixState(true)
			}
			joinErr := self.ensureJoinNamespaceGroup(nsInfo, localNamespace)
			if joinErr != nil && joinErr != ErrNamespaceConfInvalid {
				// we ensure join group as order for partitions
				break
			}
		}
	}
	return nil
}

func (self *DataCoordinator) isMeInRaftGroup(nsInfo *PartitionMetaInfo) (bool, error) {
	var lastErr error
	for _, remoteNode := range nsInfo.GetISR() {
		if remoteNode == self.GetMyID() {
			continue
		}
		nip, _, _, httpPort := ExtractNodeInfoFromID(remoteNode)
		var rsp []*common.MemberInfo
		err := common.APIRequest("GET",
			"http://"+net.JoinHostPort(nip, httpPort)+common.APIGetMembers+"/"+nsInfo.GetDesp(),
			nil, time.Second*3, &rsp)
		if err != nil {
			CoordLog().Infof("failed to get members from %v for namespace: %v, %v", nip, nsInfo.GetDesp(), err)
			lastErr = err
			continue
		}

		for _, m := range rsp {
			if m.NodeID == self.GetMyRegID() && m.ID == nsInfo.RaftIDs[self.GetMyID()] {
				return true, lastErr
			}
		}
	}
	return false, lastErr
}

func (self *DataCoordinator) isNamespaceShouldStart(nsInfo PartitionMetaInfo) bool {
	// it may happen that the node marked as removing can not be removed because
	// the raft group has not enough quorum to do the proposal to change the configure.
	// In this way we need start the removing node to join the raft group and then
	// the leader can remove the node from the raft group, and then we can safely remove
	// the removing node finally
	shouldLoad := FindSlice(nsInfo.RaftNodes, self.GetMyID()) != -1
	if !shouldLoad {
		return false
	}
	rm, ok := nsInfo.Removings[self.GetMyID()]
	if !ok {
		return true
	}
	if rm.RemoveReplicaID != nsInfo.RaftIDs[self.GetMyID()] {
		return true
	}

	inRaft, _ := self.isMeInRaftGroup(&nsInfo)
	if inRaft {
		CoordLog().Infof("removing node %v-%v should join namespace %v since still in raft",
			self.GetMyID(), rm.RemoveReplicaID, nsInfo.GetDesp())
	}
	return inRaft
}

func (self *DataCoordinator) isNamespaceShouldStop(nsInfo PartitionMetaInfo, localNamespace *node.NamespaceNode) bool {
	// removing node can stop local raft only when all the other members
	// are notified to remove this node
	// Mostly, the remove node proposal will handle the raft node stop, however
	// there are some situations to be checked here.
	rm, ok := nsInfo.Removings[self.GetMyID()]
	if !ok {
		return false
	}
	if rm.RemoveReplicaID != nsInfo.RaftIDs[self.GetMyID()] {
		return false
	}

	inRaft, err := self.isMeInRaftGroup(&nsInfo)
	if inRaft || err != nil {
		return false
	}
	CoordLog().Infof("removing node %v-%v should stop namespace %v since not in any raft group anymore",
		self.GetMyID(), rm.RemoveReplicaID, nsInfo.GetDesp())
	return false
}

func (self *DataCoordinator) checkAndFixLocalNamespaceData(nsInfo *PartitionMetaInfo, localNamespace *node.NamespaceNode) error {
	return nil
}

func (self *DataCoordinator) addNamespaceRaftMember(nsInfo *PartitionMetaInfo, m *common.MemberInfo) {
	for nid, removing := range nsInfo.Removings {
		if m.ID == removing.RemoveReplicaID && m.NodeID == ExtractRegIDFromGenID(nid) {
			CoordLog().Infof("raft member %v is marked as removing in meta: %v, ignore add raft member", m, nsInfo.Removings)
			return
		}
	}
	nsNode := self.localNSMgr.GetNamespaceNode(nsInfo.GetDesp())
	if nsNode == nil {
		CoordLog().Infof("namespace %v not found while add member", nsInfo.GetDesp())
		return
	}
	err := nsNode.Node.ProposeAddMember(*m)
	if err != nil {
		CoordLog().Infof("%v propose add %v failed: %v", nsInfo.GetDesp(), m, err)
	} else {
		CoordLog().Infof("namespace %v propose add member %v", nsInfo.GetDesp(), m)
	}
}

func (self *DataCoordinator) removeNamespaceRaftMember(nsInfo *PartitionMetaInfo, m *common.MemberInfo) {
	nsNode := self.localNSMgr.GetNamespaceNode(nsInfo.GetDesp())
	if nsNode == nil {
		CoordLog().Infof("namespace %v not found while remove member", nsInfo.GetDesp())
		return
	}

	err := nsNode.Node.ProposeRemoveMember(*m)
	if err != nil {
		CoordLog().Infof("propose remove %v failed: %v", m, err)
	} else {
		CoordLog().Infof("namespace %v propose remove member %v", nsInfo.GetDesp(), m)
	}
}

func (self *DataCoordinator) getNamespaceRaftMembers(nsInfo *PartitionMetaInfo) []*common.MemberInfo {
	nsNode := self.localNSMgr.GetNamespaceNode(nsInfo.GetDesp())
	if nsNode == nil {
		return nil
	}
	return nsNode.Node.GetMembers()
}

func (self *DataCoordinator) getNamespaceRaftLeader(nsInfo *PartitionMetaInfo) uint64 {
	nsNode := self.localNSMgr.GetNamespaceNode(nsInfo.GetDesp())
	if nsNode == nil {
		return 0
	}
	m := nsNode.Node.GetLeadMember()
	if m == nil {
		return 0
	}
	return m.NodeID
}

func (self *DataCoordinator) transferMyNamespaceLeader(nsInfo *PartitionMetaInfo, nid string) {
	nsNode := self.localNSMgr.GetNamespaceNode(nsInfo.GetDesp())
	if nsNode == nil {
		return
	}
	toRaftID, ok := nsInfo.RaftIDs[nid]
	if !ok {
		CoordLog().Warningf("transfer namespace %v leader to %v failed for missing raft id: %v",
			nsInfo.GetDesp(), nid, nsInfo.RaftIDs)
		return
	}
	CoordLog().Infof("begin transfer namespace %v leader to %v", nsInfo.GetDesp(), nid)
	err := nsNode.TransferMyLeader(ExtractRegIDFromGenID(nid), toRaftID)
	if err != nil {
		CoordLog().Infof("failed to transfer namespace %v leader to %v: %v", nsInfo.GetDesp(), nid, err)
	}
}

func (self *DataCoordinator) checkForUnsyncedNamespaces() {
	ticker := time.NewTicker(time.Minute * 10)
	defer self.wg.Done()
	doWork := func() {
		// try load local namespace if any namespace raft group changed
		self.loadLocalNamespaceData()

		// check local namespaces with cluster to remove the unneed data
		tmpChecks := self.localNSMgr.GetNamespaces()
		for name, localNamespace := range tmpChecks {
			namespace, pid := common.GetNamespaceAndPartition(name)
			if namespace == "" {
				CoordLog().Warningf("namespace invalid: %v", name)
				continue
			}
			namespaceMeta, err := self.register.GetNamespacePartInfo(namespace, pid)
			if err != nil {
				if err == ErrKeyNotFound {
					CoordLog().Infof("the namespace should be clean since not found in register: %v", name)
					_, err = self.register.GetNamespaceMetaInfo(namespace)
					if err == ErrKeyNotFound {
						self.removeLocalNamespaceFromRaft(localNamespace, true)
						self.forceRemoveLocalNamespace(localNamespace)
					}
				}
				go self.tryCheckNamespaces()
				continue
			}
			if self.isNamespaceShouldStop(*namespaceMeta, localNamespace) {
				self.forceRemoveLocalNamespace(localNamespace)
				continue
			}
			leader := self.getNamespaceRaftLeader(namespaceMeta)
			if leader == 0 {
				continue
			}
			isrList := namespaceMeta.GetISR()
			if FindSlice(isrList, self.myNode.GetID()) == -1 {
				if len(isrList) > 0 {
					CoordLog().Infof("the namespace should be clean : %v", namespaceMeta)
					self.removeLocalNamespaceFromRaft(localNamespace, true)
				}
				continue
			}
			// only leader check the follower status
			if leader != self.GetMyRegID() || len(isrList) == 0 {
				continue
			}
			isReplicasEnough := len(isrList) >= namespaceMeta.Replica

			if isReplicasEnough && isrList[0] != self.GetMyID() {
				// the raft leader check if I am the expected sharding leader,
				// if not, try to transfer the leader to expected node. We need do this
				// because we should make all the sharding leaders balanced on
				// all the cluster nodes.
				self.transferMyNamespaceLeader(namespaceMeta, isrList[0])
			} else {
				members := self.getNamespaceRaftMembers(namespaceMeta)
				// check if any replica is not joined to members
				anyJoined := false
				for nid, rid := range namespaceMeta.RaftIDs {
					found := false
					for _, m := range members {
						if m.ID == rid {
							found = true
							if m.NodeID != ExtractRegIDFromGenID(nid) {
								CoordLog().Infof("found raft member %v mismatch the replica node: %v", m, nid)
							}
							break
						}
					}
					if !found {
						anyJoined = true
						var m common.MemberInfo
						m.ID = rid
						m.NodeID = ExtractRegIDFromGenID(nid)
						m.GroupID = uint64(namespaceMeta.MinGID) + uint64(namespaceMeta.Partition)
						m.GroupName = namespaceMeta.GetDesp()
						raddr, err := self.getRaftAddrForNode(nid)
						if err != nil {
							CoordLog().Infof("failed to get raft address for node: %v, %v", nid, err)
						} else {
							m.RaftURLs = append(m.RaftURLs, raddr)
							self.addNamespaceRaftMember(namespaceMeta, &m)
						}
					}
				}
				if anyJoined || len(members) <= namespaceMeta.Replica || !isReplicasEnough {
					go self.tryCheckNamespaces()
					continue
				}
				// the members is more than replica, we need to remove the member that is not necessary anymore
				for _, m := range members {
					found := false
					for nid, rid := range namespaceMeta.RaftIDs {
						if m.ID == rid {
							found = true
							if m.NodeID != ExtractRegIDFromGenID(nid) {
								CoordLog().Infof("found raft member %v mismatch the replica node: %v", m, nid)
							}
							break
						}
					}
					if !found {
						CoordLog().Infof("raft member %v not found in meta: %v", m, namespaceMeta.RaftNodes)
						self.removeNamespaceRaftMember(namespaceMeta, m)
					} else {
						for nid, removing := range namespaceMeta.Removings {
							if m.ID == removing.RemoveReplicaID && m.NodeID == ExtractRegIDFromGenID(nid) {
								CoordLog().Infof("raft member %v is marked as removing in meta: %v", m, namespaceMeta.Removings)
								self.removeNamespaceRaftMember(namespaceMeta, m)
							}
						}
					}
				}
			}
		}
	}

	nsChangedChan := self.register.GetNamespacesNotifyChan()
	for {
		select {
		case <-self.stopChan:
			return
		case <-self.tryCheckUnsynced:
			doWork()
		case <-ticker.C:
			doWork()
		case <-nsChangedChan:
			doWork()
		}
	}
}

func (self *DataCoordinator) forceRemoveLocalNamespace(localNamespace *node.NamespaceNode) {
	err := localNamespace.Destroy()
	if err != nil {
		CoordLog().Infof("failed to force remove local data: %v", err)
	}
}

func (self *DataCoordinator) removeLocalNamespaceFromRaft(localNamespace *node.NamespaceNode, removeData bool) *CoordErr {
	if removeData {
		if !localNamespace.IsReady() {
			return ErrNamespaceNotReady
		}
		m := localNamespace.Node.GetLocalMemberInfo()
		CoordLog().Infof("removing %v from namespace : %v", m.ID, m.GroupName)

		localErr := localNamespace.Node.ProposeRemoveMember(*m)
		if localErr != nil {
			CoordLog().Infof("propose remove self %v failed : %v", m, localErr)
			return &CoordErr{localErr.Error(), RpcCommonErr, CoordLocalErr}
		}
	} else {
		if localNamespace == nil {
			return ErrNamespaceNotCreated
		}
		localNamespace.Close()
	}
	return nil
}

func (self *DataCoordinator) getRaftAddrForNode(nid string) (string, *CoordErr) {
	node, err := self.register.GetNodeInfo(nid)
	if err != nil {
		return "", &CoordErr{err.Error(), RpcNoErr, CoordRegisterErr}
	}
	return node.RaftTransportAddr, nil
}

func (self *DataCoordinator) prepareNamespaceConf(nsInfo *PartitionMetaInfo) (*node.NamespaceConfig, *CoordErr) {
	raftID, ok := nsInfo.RaftIDs[self.GetMyID()]
	if !ok {
		CoordLog().Warningf("namespace %v has no raft id for local: %v", nsInfo.GetDesp(), nsInfo)
		return nil, ErrNamespaceConfInvalid
	}
	var err *CoordErr
	nsConf := node.NewNSConfig()
	nsConf.BaseName = nsInfo.Name
	nsConf.Name = nsInfo.GetDesp()
	nsConf.EngType = nsInfo.EngType
	nsConf.PartitionNum = nsInfo.PartitionNum
	nsConf.Replicator = nsInfo.Replica
	nsConf.RaftGroupConf.GroupID = uint64(nsInfo.MinGID) + uint64(nsInfo.Partition)
	nsConf.RaftGroupConf.SeedNodes = make([]node.ReplicaInfo, 0)
	for _, nid := range nsInfo.GetISR() {
		var rinfo node.ReplicaInfo
		if nid == self.GetMyID() {
			rinfo.NodeID = self.GetMyRegID()
			rinfo.ReplicaID = raftID
			rinfo.RaftAddr = self.myNode.RaftTransportAddr
		} else {
			rinfo.NodeID = ExtractRegIDFromGenID(nid)
			rid, ok := nsInfo.RaftIDs[nid]
			if !ok {
				CoordLog().Infof("can not found raft id for node: %v, %v", nid, nsInfo.RaftIDs)
				continue
			}
			rinfo.ReplicaID = rid
			rinfo.RaftAddr, err = self.getRaftAddrForNode(nid)
			if err != nil {
				CoordLog().Infof("can not found raft address for node: %v, %v", nid, err)
				continue
			}
		}
		nsConf.RaftGroupConf.SeedNodes = append(nsConf.RaftGroupConf.SeedNodes, rinfo)
	}
	if len(nsConf.RaftGroupConf.SeedNodes) == 0 {
		CoordLog().Warningf("can not found any seed nodes for namespace: %v", nsInfo)
		return nil, ErrNamespaceConfInvalid
	}
	return nsConf, nil
}

func (self *DataCoordinator) requestJoinNamespaceGroup(raftID uint64, nsInfo *PartitionMetaInfo,
	localNamespace *node.NamespaceNode, remoteNode string) error {
	var m common.MemberInfo
	m.ID = raftID
	m.NodeID = self.GetMyRegID()
	m.GroupID = uint64(nsInfo.MinGID) + uint64(nsInfo.Partition)
	m.GroupName = nsInfo.GetDesp()
	localNamespace.Node.FillMyMemberInfo(&m)
	CoordLog().Infof("request to %v for join member: %v", remoteNode, m)
	if remoteNode == self.GetMyID() {
		return nil
	}
	nip, _, _, httpPort := ExtractNodeInfoFromID(remoteNode)
	d, _ := json.Marshal(m)
	err := common.APIRequest("POST",
		"http://"+net.JoinHostPort(nip, httpPort)+common.APIAddNode,
		bytes.NewReader(d), time.Second*3, nil)
	if err != nil {
		CoordLog().Infof("failed to request join namespace: %v", err)
		return err
	}
	return nil
}

func (self *DataCoordinator) tryCheckNamespaces() {
	time.Sleep(time.Second)
	select {
	case self.tryCheckUnsynced <- true:
	default:
	}
}

func (self *DataCoordinator) ensureJoinNamespaceGroup(nsInfo PartitionMetaInfo,
	localNamespace *node.NamespaceNode) *CoordErr {

	if rm, ok := nsInfo.Removings[self.GetMyID()]; ok {
		if rm.RemoveReplicaID == nsInfo.RaftIDs[self.GetMyID()] {
			CoordLog().Infof("ignore join namespace %v since it removing node: %v", nsInfo.GetDesp(), rm)
			return nil
		}
	}
	// check if in local raft group
	myRunning := atomic.AddInt32(&self.catchupRunning, 1)
	defer atomic.AddInt32(&self.catchupRunning, -1)
	if myRunning > MAX_RAFT_JOIN_RUNNING {
		CoordLog().Infof("catching too much running: %v", myRunning)
		self.tryCheckNamespaces()
		return ErrCatchupRunningBusy
	}

	dyConf := &node.NamespaceDynamicConf{}
	localNamespace.SetDynamicInfo(*dyConf)
	if localNamespace.IsDataNeedFix() {
		// clean local data
	}
	localNamespace.SetDataFixState(false)
	raftID, ok := nsInfo.RaftIDs[self.GetMyID()]
	if !ok {
		CoordLog().Warningf("namespace %v failed to get raft id %v while check join", nsInfo.GetDesp(),
			nsInfo.RaftIDs)
		return ErrNamespaceConfInvalid
	}
	var joinErr *CoordErr
	retry := 0
	startCheck := time.Now()
	for time.Since(startCheck) < time.Second*30 {
		mems := localNamespace.GetMembers()
		memsMap := make(map[uint64]*common.MemberInfo)
		alreadyJoined := false
		for _, m := range mems {
			memsMap[m.NodeID] = m
			if m.NodeID == self.GetMyRegID() &&
				m.GroupName == nsInfo.GetDesp() &&
				m.ID == raftID {
				if len(mems) > len(nsInfo.GetISR())/2 {
					alreadyJoined = true
				} else {
					CoordLog().Infof("namespace %v is in the small raft group %v, need join large group:%v",
						nsInfo.GetDesp(), mems, nsInfo.RaftNodes)
				}
			}
		}
		if alreadyJoined {
			if localNamespace.IsRaftSynced() {
				joinErr = nil
				break
			}
			CoordLog().Infof("namespace %v still waiting raft synced", nsInfo.GetDesp())
			select {
			case <-self.stopChan:
				return ErrNamespaceExiting
			case <-time.After(time.Second / 2):
			}
			joinErr = ErrNamespaceWaitingSync
		} else {
			joinErr = ErrNamespaceWaitingSync
			var remote string
			cnt := 0
			isr := nsInfo.GetISR()
			if len(isr) == 0 {
				isr = nsInfo.RaftNodes
			}
			for cnt <= len(isr) {
				remote = isr[retry%len(isr)]
				retry++
				cnt++
				if remote == self.GetMyID() {
					continue
				}
				if _, ok := memsMap[ExtractRegIDFromGenID(remote)]; !ok {
					break
				}
			}
			self.requestJoinNamespaceGroup(raftID, &nsInfo, localNamespace, remote)
			select {
			case <-self.stopChan:
				return ErrNamespaceExiting
			case <-time.After(time.Second / 2):
			}
		}
	}
	if joinErr != nil {
		self.tryCheckNamespaces()
		CoordLog().Infof("local namespace join failed: %v, retry later: %v", joinErr, nsInfo.GetDesp())
	} else if retry > 0 {
		CoordLog().Infof("local namespace join done: %v", nsInfo.GetDesp())
	}
	return joinErr
}

func (self *DataCoordinator) updateLocalNamespace(nsInfo *PartitionMetaInfo) (*node.NamespaceNode, *CoordErr) {
	// check namespace exist and prepare on local.
	raftID, ok := nsInfo.RaftIDs[self.GetMyID()]
	if !ok {
		CoordLog().Warningf("namespace %v has no raft id for local", nsInfo.GetDesp(), nsInfo.RaftIDs)
		return nil, ErrNamespaceConfInvalid
	}
	nsConf, err := self.prepareNamespaceConf(nsInfo)
	if err != nil {
		CoordLog().Warningf("prepare join namespace %v failed: %v", nsInfo.GetDesp(), err)
		return nil, err
	}

	localNode, _ := self.localNSMgr.InitNamespaceNode(nsConf, raftID)
	if localNode != nil {
		if checkErr := localNode.CheckRaftConf(raftID, nsConf); checkErr != nil {
			CoordLog().Infof("local namespace %v mismatch with the new raft config removing: %v", nsInfo.GetDesp(), checkErr)
			return nil, &CoordErr{checkErr.Error(), RpcNoErr, CoordLocalErr}
		}
	}
	if localNode == nil {
		CoordLog().Warningf("local namespace %v init failed", nsInfo.GetDesp())
		return nil, ErrLocalInitNamespaceFailed
	}

	localErr := localNode.SetMagicCode(nsInfo.MagicCode)
	if localErr != nil {
		CoordLog().Warningf("local namespace %v init magic code failed: %v", nsInfo.GetDesp(), localErr)
		return localNode, ErrLocalInitNamespaceFailed
	}
	dyConf := &node.NamespaceDynamicConf{}
	localNode.SetDynamicInfo(*dyConf)
	if err := localNode.Start(); err != nil {
		return nil, ErrLocalInitNamespaceFailed
	}
	return localNode, nil
}

func (self *DataCoordinator) GetSnapshotSyncInfo(fullNamespace string) ([]common.SnapshotSyncInfo, error) {
	namespace, pid := common.GetNamespaceAndPartition(fullNamespace)
	if namespace == "" {
		CoordLog().Warningf("namespace invalid: %v", fullNamespace)
		return nil, nil
	}
	nsInfo, err := self.register.GetNamespacePartInfo(namespace, pid)
	if err != nil {
		return nil, err
	}
	var ssiList []common.SnapshotSyncInfo
	for _, nid := range nsInfo.GetISR() {
		node, err := self.register.GetNodeInfo(nid)
		if err != nil {
			continue
		}
		var ssi common.SnapshotSyncInfo
		ssi.NodeID = node.RegID
		ssi.DataRoot = node.DataRoot
		ssi.ReplicaID = nsInfo.RaftIDs[nid]
		ssi.RemoteAddr = node.NodeIP
		ssi.HttpAPIPort = node.HttpPort
		ssi.RsyncModule = node.RsyncModule
		ssiList = append(ssiList, ssi)
	}
	return ssiList, nil
}

// before shutdown, we transfer the leader to others to reduce
// the unavailable time.
func (self *DataCoordinator) prepareLeavingCluster() {
	CoordLog().Infof("I am prepare leaving the cluster.")
	allNamespaces, _, _ := self.register.GetAllNamespaces()
	for _, nsParts := range allNamespaces {
		for _, nsInfo := range nsParts {
			if FindSlice(nsInfo.RaftNodes, self.myNode.GetID()) == -1 {
				continue
			}
			localNamespace := self.localNSMgr.GetNamespaceNode(nsInfo.GetDesp())
			if localNamespace == nil {
				continue
			}
			// only leader check the follower status
			leader := self.getNamespaceRaftLeader(nsInfo.GetCopy())
			if leader != self.GetMyRegID() {
				continue
			}
			for _, newLeader := range nsInfo.GetISR() {
				if newLeader == self.GetMyID() {
					continue
				}
				self.transferMyNamespaceLeader(nsInfo.GetCopy(), newLeader)
				break
			}
		}
	}
	CoordLog().Infof("prepare leaving finished.")
	self.localNSMgr.Stop()
	if self.register != nil {
		atomic.StoreInt32(&self.stopping, 1)
		self.register.Unregister(&self.myNode)
		self.register.Stop()
	}
}

func (self *DataCoordinator) Stats(namespace string, part int) *CoordStats {
	s := &CoordStats{}
	s.NsCoordStats = make([]NamespaceCoordStat, 0)
	if len(namespace) > 0 {
		meta, err := self.register.GetNamespaceMetaInfo(namespace)
		if err != nil {
			CoordLog().Infof("failed to get namespace info: %v", err)
			return s
		}
		if part >= 0 {
			nsInfo, err := self.register.GetNamespacePartInfo(namespace, part)
			if err != nil {
			} else {
				var stat NamespaceCoordStat
				stat.Name = namespace
				stat.Partition = part
				for _, nid := range nsInfo.RaftNodes {
					stat.ISRStats = append(stat.ISRStats, ISRStat{HostName: "", NodeID: nid})
				}
				s.NsCoordStats = append(s.NsCoordStats, stat)
			}
		} else {
			for i := 0; i < meta.PartitionNum; i++ {
				nsInfo, err := self.register.GetNamespacePartInfo(namespace, part)
				if err != nil {
					continue
				}
				var stat NamespaceCoordStat
				stat.Name = namespace
				stat.Partition = nsInfo.Partition
				for _, nid := range nsInfo.RaftNodes {
					stat.ISRStats = append(stat.ISRStats, ISRStat{HostName: "", NodeID: nid})
				}
				s.NsCoordStats = append(s.NsCoordStats, stat)
			}
		}
	}
	return s
}