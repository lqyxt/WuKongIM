package cluster

import (
	"context"
	"errors"
	"fmt"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WuKongIM/WuKongIM/pkg/cluster/clusterconfig/pb"
	"github.com/WuKongIM/WuKongIM/pkg/cluster/clusterevent"
	"github.com/WuKongIM/WuKongIM/pkg/cluster/icluster"
	"github.com/WuKongIM/WuKongIM/pkg/cluster/reactor"
	"github.com/WuKongIM/WuKongIM/pkg/keylock"
	"github.com/WuKongIM/WuKongIM/pkg/trace"
	"github.com/WuKongIM/WuKongIM/pkg/wklog"
	"github.com/WuKongIM/WuKongIM/pkg/wknet"
	"github.com/WuKongIM/WuKongIM/pkg/wkserver"
	"github.com/WuKongIM/WuKongIM/pkg/wkserver/proto"
	"github.com/WuKongIM/WuKongIM/pkg/wkutil"
	"github.com/bwmarrin/snowflake"
	"github.com/lni/goutils/syncutil"
	"github.com/panjf2000/ants/v2"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

var _ icluster.Cluster = (*Server)(nil)

type Server struct {
	opts               *Options
	clusterEventServer *clusterevent.Server // 分布式配置事件中心
	nodeManager        *nodeManager         // 节点管理者
	slotManager        *slotManager         // 槽管理者
	channelManager     *channelManager      // 频道管理者

	channelKeyLock         *keylock.KeyLock        // 频道锁
	netServer              *wkserver.Server        // 节点之间通讯的网络服务
	channelElectionPool    *ants.Pool              // 频道选举的协程池
	channelElectionManager *channelElectionManager // 频道选举管理者
	channelLoadPool        *ants.Pool              // 加载频道的协程池
	channelLoadMap         map[string]struct{}     // 频道是否在加载中的map
	channelLoadMapLock     sync.RWMutex            // 频道是否在加载中的map锁
	cancelCtx              context.Context
	cancelFnc              context.CancelFunc
	onMessageFnc           func(msg *proto.Message) // 上层处理消息的函数
	logIdGen               *snowflake.Node          // 日志id生成
	slotStorage            *PebbleShardLogStorage
	apiPrefix              string    // api前缀
	uptime                 time.Time // 服务器启动时间
	wklog.Log

	stopped atomic.Bool

	stopper *syncutil.Stopper
}

func New(opts *Options) *Server {

	s := &Server{
		opts:           opts,
		nodeManager:    newNodeManager(opts),
		slotManager:    newSlotManager(opts),
		channelManager: newChannelManager(opts),
		Log:            wklog.NewWKLog(fmt.Sprintf("cluster[%d]", opts.NodeId)),
		channelKeyLock: keylock.NewKeyLock(),
		channelLoadMap: make(map[string]struct{}),
		stopper:        syncutil.NewStopper(),
	}

	if opts.SlotLogStorage == nil {
		s.slotStorage = NewPebbleShardLogStorage(path.Join(opts.DataDir, "logdb"))
		opts.SlotLogStorage = s.slotStorage
	}

	logIdGen, err := snowflake.NewNode(int64(opts.NodeId))
	if err != nil {
		s.Panic("new logIdGen failed", zap.Error(err))
	}
	s.logIdGen = logIdGen

	cfgDir := path.Join(opts.DataDir, "config")

	initNodes := opts.InitNodes
	if len(initNodes) == 0 {
		if strings.TrimSpace(s.opts.Seed) != "" {
			seedNodeID, seedAddr, err := seedNode(s.opts.Seed)
			if err != nil {
				panic(err)
			}
			initNodes[seedNodeID] = seedAddr
		}
		initNodes[s.opts.NodeId] = strings.ReplaceAll(s.opts.Addr, "tcp://", "")
	}

	opts.Send = s.send
	s.clusterEventServer = clusterevent.New(clusterevent.NewOptions(
		clusterevent.WithNodeId(opts.NodeId),
		clusterevent.WithInitNodes(initNodes),
		clusterevent.WithSlotCount(opts.SlotCount),
		clusterevent.WithSlotMaxReplicaCount(opts.SlotMaxReplicaCount),
		clusterevent.WithChannelMaxReplicaCount(uint32(opts.ChannelMaxReplicaCount)),
		clusterevent.WithReady(s.onEvent),
		clusterevent.WithSend(s.onSend),
		clusterevent.WithConfigDir(cfgDir),
		clusterevent.WithApiServerAddr(opts.ApiServerAddr),
	))

	channelElectionPool, err := ants.NewPool(s.opts.ChannelElectionPoolSize, ants.WithNonblocking(false), ants.WithDisablePurge(true), ants.WithPanicHandler(func(err interface{}) {
		stack := debug.Stack()
		s.Panic("频道选举协程池崩溃", zap.Error(err.(error)), zap.String("stack", string(stack)))
	}))
	if err != nil {
		s.Panic("new channelElectionPool failed", zap.Error(err))
	}
	s.channelElectionPool = channelElectionPool

	s.channelLoadPool, err = ants.NewPool(s.opts.ChannelLoadPoolSize, ants.WithNonblocking(true), ants.WithPanicHandler(func(err interface{}) {
		stack := debug.Stack()
		s.Panic("频道加载协程池崩溃", zap.Error(err.(error)), zap.String("stack", string(stack)))
	}))
	if err != nil {
		s.Panic("new channelLoadPool failed", zap.Error(err))
	}

	s.netServer = wkserver.New(
		opts.Addr, wkserver.WithMessagePoolOn(false),
		wkserver.WithOnRequest(func(conn wknet.Conn, req *proto.Request) {
			trace.GlobalTrace.Metrics.System().IntranetIncomingAdd(int64(len(req.Body)))
		}),
		wkserver.WithOnResponse(func(conn wknet.Conn, resp *proto.Response) {
			trace.GlobalTrace.Metrics.System().IntranetOutgoingAdd(int64(len(resp.Body)))
		}),
	)
	s.channelElectionManager = newChannelElectionManager(s)
	s.cancelCtx, s.cancelFnc = context.WithCancel(context.Background())
	return s
}

func (s *Server) Start() error {

	s.uptime = time.Now()

	err := s.slotStorage.Open()
	if err != nil {
		return err
	}

	s.channelKeyLock.StartCleanLoop()

	nodes := s.clusterEventServer.Nodes()
	if len(nodes) > 0 {
		for _, node := range nodes {
			if node.Id == s.opts.NodeId {
				continue
			}
			s.nodeManager.addNode(s.newNodeByNodeInfo(node.Id, node.ClusterAddr))
		}
	} else if len(s.opts.InitNodes) > 0 {
		for nodeId, clusterAddr := range s.opts.InitNodes {
			if nodeId == s.opts.NodeId {
				continue
			}
			s.nodeManager.addNode(s.newNodeByNodeInfo(nodeId, clusterAddr))
		}
	}

	// 分布式事件开启
	slots := s.clusterEventServer.Slots()
	for _, slot := range slots {
		if !wkutil.ArrayContainsUint64(slot.Replicas, s.opts.NodeId) {
			continue
		}
		s.addSlot(slot)
	}

	// channel election manager
	err = s.channelElectionManager.start()
	if err != nil {
		return err
	}

	// cluster event server
	err = s.clusterEventServer.Start()
	if err != nil {
		return err
	}

	// net server
	s.setRoutes()
	s.netServer.OnMessage(s.onMessage)
	err = s.netServer.Start()
	if err != nil {
		return err
	}
	// slot manager
	err = s.slotManager.start()
	if err != nil {
		return err
	}
	// channel manager
	err = s.channelManager.start()
	if err != nil {
		return err
	}

	// 如果有新加入的节点 则执行加入逻辑
	if s.needJoin() { // 需要加入集群
		s.clusterEventServer.SetIsPrepared(false) // 先将节点集群准备状态设置为false，等待加入集群后再设置为true
		s.stopper.RunWorker(s.joinLoop)
	}

	return nil
}

func (s *Server) Stop() {
	s.stopped.Store(true)
	s.cancelFnc()
	s.stopper.Stop()
	s.nodeManager.stop()
	s.channelElectionManager.stop()
	s.netServer.Stop()
	s.clusterEventServer.Stop()
	s.slotManager.stop()
	s.channelManager.stop()
	s.channelKeyLock.StopCleanLoop()
	s.slotStorage.Close()

}

func (s *Server) AddSlotMessage(m reactor.Message) {

	// 统计引入的消息
	traceIncomingMessage(trace.ClusterKindSlot, m.MsgType, int64(m.Size()))

	s.slotManager.addMessage(m)
}

func (s *Server) AddConfigMessage(m reactor.Message) {

	s.clusterEventServer.AddMessage(m)
}

func (s *Server) AddChannelMessage(m reactor.Message) {

	// 统计引入的消息
	traceIncomingMessage(trace.ClusterKindChannel, m.MsgType, int64(m.Size()))

	ch := s.channelManager.getWithHandleKey(m.HandlerKey)
	if ch != nil {
		s.channelManager.addMessage(m)
		return
	}

	s.channelLoadMapLock.RLock()
	if _, ok := s.channelLoadMap[m.HandlerKey]; ok {
		s.channelLoadMapLock.RUnlock()
		return
	}
	s.channelLoadMapLock.RUnlock()

	s.channelLoadMapLock.Lock()
	s.channelLoadMap[m.HandlerKey] = struct{}{}
	s.channelLoadMapLock.Unlock()

	running := s.channelLoadPool.Running()
	if running > s.opts.ChannelLoadPoolSize-10 {
		s.Warn("channelLoadPool is busy", zap.Int("running", running), zap.Int("size", s.opts.ChannelLoadPoolSize))
	}
	err := s.channelLoadPool.Submit(func() {
		channelId, channelType := ChannelFromlKey(m.HandlerKey)
		if channelId == "" {
			s.Panic("channelId is empty", zap.String("handlerKey", m.HandlerKey))
		}
		_, err := s.loadOrCreateChannel(s.cancelCtx, channelId, channelType)
		if err != nil {
			s.Error("loadOrCreateChannel failed", zap.Error(err), zap.String("handlerKey", m.HandlerKey), zap.Uint64("from", m.From), zap.String("msgType", m.MsgType.String()))
		}
		s.channelLoadMapLock.Lock()
		delete(s.channelLoadMap, m.HandlerKey)
		s.channelLoadMapLock.Unlock()

		s.Debug("active channel", zap.String("handlerKey", m.HandlerKey), zap.Uint64("from", m.From), zap.String("msgType", m.MsgType.String()))
	})
	if err != nil {
		s.Error("channelLoadPool.Submit failed", zap.Error(err))
	}
}

func (s *Server) newNodeByNodeInfo(nodeID uint64, addr string) *node {
	n := newNode(nodeID, s.serverUid(s.opts.NodeId), addr, s.opts)
	n.start()
	return n
}

func (s *Server) newSlot(st *pb.Slot) *slot {

	slot := newSlot(st, s.opts)
	return slot

}

func (s *Server) addSlot(slot *pb.Slot) {
	st := s.newSlot(slot)
	s.slotManager.add(st)
	st.update(slot)
}

func (s *Server) addOrUpdateSlot(st *pb.Slot) {
	handler := s.slotManager.get(st.Id)
	if handler == nil {
		s.addSlot(st)
		return
	}
	handler.(*slot).update(st)

}

func (s *Server) serverUid(id uint64) string {
	return fmt.Sprintf("%d", id)
}

func (s *Server) send(shardType ShardType, m reactor.Message) {
	if s.stopped.Load() {
		return
	}
	// 输出消息统计
	if shardType == ShardTypeSlot {
		traceOutgoingMessage(trace.ClusterKindSlot, m.MsgType, int64(m.Size()))
	} else if shardType == ShardTypeChannel {
		traceOutgoingMessage(trace.ClusterKindChannel, m.MsgType, int64(m.Size()))
	}

	node := s.nodeManager.node(m.To)
	if node == nil {
		s.Warn("send failed, node not exist", zap.Uint64("to", m.To))
		return
	}
	data, err := m.Marshal()
	if err != nil {
		s.Error("Marshal failed", zap.Error(err))
		return
	}

	var msgType uint32
	if shardType == ShardTypeSlot {
		msgType = MsgTypeSlot
	} else if shardType == ShardTypeConfig {
		msgType = MsgTypeConfig
	} else if shardType == ShardTypeChannel {
		msgType = MsgTypeChannel
	} else {
		s.Error("send failed, invalid shardType", zap.Uint8("shardType", uint8(shardType)))
		return
	}
	msg := &proto.Message{
		MsgType: msgType,
		Content: data,
	}

	switch msg.MsgType {
	case MsgTypeChannel:
		trace.GlobalTrace.Metrics.Cluster().MessageOutgoingBytesAdd(trace.ClusterKindChannel, int64(msg.Size()))
		trace.GlobalTrace.Metrics.Cluster().MessageOutgoingCountAdd(trace.ClusterKindChannel, 1)
	case MsgTypeSlot:
		trace.GlobalTrace.Metrics.Cluster().MessageOutgoingBytesAdd(trace.ClusterKindSlot, int64(msg.Size()))
		trace.GlobalTrace.Metrics.Cluster().MessageOutgoingCountAdd(trace.ClusterKindSlot, 1)
	case MsgTypeConfig:
		trace.GlobalTrace.Metrics.Cluster().MessageOutgoingBytesAdd(trace.ClusterKindConfig, int64(msg.Size()))
		trace.GlobalTrace.Metrics.Cluster().MessageOutgoingCountAdd(trace.ClusterKindConfig, 1)
	}

	err = node.send(msg)
	if err != nil {
		s.Error("send failed", zap.Error(err))
		return
	}
}

func (s *Server) onSend(m reactor.Message) {

	s.opts.Send(ShardTypeConfig, m)
}

func (s *Server) onMessage(c wknet.Conn, m *proto.Message) {
	if s.stopped.Load() {
		return
	}
	msgSize := int64(m.Size())

	trace.GlobalTrace.Metrics.System().IntranetIncomingAdd(msgSize) // 内网流量统计

	switch m.MsgType {
	case MsgTypeConfig:
		msg, err := reactor.UnmarshalMessage(m.Content)
		if err != nil {
			s.Error("UnmarshalMessage failed", zap.Error(err))
			return
		}
		trace.GlobalTrace.Metrics.Cluster().MessageIncomingCountAdd(trace.ClusterKindConfig, 1)
		trace.GlobalTrace.Metrics.Cluster().MessageIncomingBytesAdd(trace.ClusterKindConfig, msgSize)
		s.AddConfigMessage(msg)
	case MsgTypeSlot:
		msg, err := reactor.UnmarshalMessage(m.Content)
		if err != nil {
			s.Error("UnmarshalMessage failed", zap.Error(err))
			return
		}
		trace.GlobalTrace.Metrics.Cluster().MessageIncomingCountAdd(trace.ClusterKindSlot, 1)
		trace.GlobalTrace.Metrics.Cluster().MessageIncomingBytesAdd(trace.ClusterKindSlot, msgSize)
		s.AddSlotMessage(msg)
	case MsgTypeChannel:
		msg, err := reactor.UnmarshalMessage(m.Content)
		if err != nil {
			s.Error("UnmarshalMessage failed", zap.Error(err))
			return
		}
		trace.GlobalTrace.Metrics.Cluster().MessageIncomingCountAdd(trace.ClusterKindChannel, 1)
		trace.GlobalTrace.Metrics.Cluster().MessageIncomingBytesAdd(trace.ClusterKindChannel, msgSize)
		s.AddChannelMessage(msg)
	default:
		trace.GlobalTrace.Metrics.Cluster().MessageIncomingCountAdd(trace.ClusterKindUnknown, 1)
		trace.GlobalTrace.Metrics.Cluster().MessageIncomingBytesAdd(trace.ClusterKindUnknown, msgSize)
		if s.onMessageFnc != nil {
			fmt.Println("msg.MsgType---->", m.MsgType)
			go s.onMessageFnc(m) // TODO: 这里需要优化
		}
	}
}

// 获取频道所在的slotId
func (s *Server) getSlotId(v string) uint32 {
	var slotCount uint32 = s.clusterEventServer.SlotCount()
	if slotCount == 0 {
		slotCount = s.opts.SlotCount
	}
	return wkutil.GetSlotNum(int(slotCount), v)
}

// 是否是单机模式
func (s *Server) isSingleNode() bool {
	return strings.TrimSpace(s.opts.Seed) == "" && len(s.opts.InitNodes) == 0
}

// 是否需要加入集群
func (s *Server) needJoin() bool {
	if strings.TrimSpace(s.opts.Seed) == "" {
		return false
	}
	seedNodeId, _, _ := seedNode(s.opts.Seed) // New里已经验证过seed了  所以这里不必再处理error了
	seedNode := s.clusterEventServer.Node(seedNodeId)
	return seedNode == nil
}

func (s *Server) joinLoop() {
	seedNodeId, _, _ := seedNode(s.opts.Seed)
	req := &ClusterJoinReq{
		NodeId:     s.opts.NodeId,
		ServerAddr: s.opts.ServerAddr,
		Role:       s.opts.Role,
	}
	for {
		select {
		case <-time.After(time.Second * 2):
			resp, err := s.nodeManager.requestClusterJoin(seedNodeId, req)
			if err != nil {
				s.Error("requestClusterJoin failed", zap.Error(err))
				continue
			}
			if len(resp.Nodes) > 0 {
				for _, n := range resp.Nodes {
					s.addOrUpdateNode(n.NodeId, n.ServerAddr)
				}
			}
			s.clusterEventServer.SetIsPrepared(true)
			return
		case <-s.stopper.ShouldStop():
			return
		}
	}
}

func seedNode(seed string) (uint64, string, error) {
	seedArray := strings.Split(seed, "@")
	if len(seedArray) < 2 {
		return 0, "", errors.New("seed format error")
	}
	seedNodeIDStr := seedArray[0]
	seedAddr := seedArray[1]
	seedNodeID, err := strconv.ParseUint(seedNodeIDStr, 10, 64)
	if err != nil {
		return 0, "", err
	}
	return seedNodeID, seedAddr, nil
}