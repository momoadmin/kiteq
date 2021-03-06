package core

import (
	"errors"
	"kiteq/binding"
	"kiteq/client/chandler"
	"kiteq/client/listener"
	"kiteq/pipe"
	rclient "kiteq/remoting/client"
	"kiteq/stat"
	"log"
	"math/rand"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const MAX_CLIENT_CONN = 10

type KiteClientManager struct {
	ga *rclient.GroupAuth

	topics        []string
	binds         []*binding.Binding //订阅的关系
	clientManager *rclient.ClientManager
	flowControl   *stat.FlowControl
	kiteClients   map[string] /*topic*/ []*kiteClient //topic对应的kiteclient
	zkManager     *binding.ZKManager
	pipeline      *pipe.DefaultPipeline
	lock          sync.RWMutex
}

func NewKiteClientManager(zkAddr, groupId, secretKey string, listen listener.IListener) *KiteClientManager {

	//重连管理器
	reconnManager := rclient.NewReconnectManager(true, 30*time.Second, 100, handshake)
	reconnManager.Start()
	//流量
	flowControl := stat.NewFlowControl("kiteclient-" + groupId)
	//构造pipeline的结构
	pipeline := pipe.NewDefaultPipeline()
	clientm := rclient.NewClientManager(reconnManager)
	pipeline.RegisteHandler("kiteclient-packet", chandler.NewPacketHandler("kiteclient-packet", flowControl))
	pipeline.RegisteHandler("kiteclient-heartbeat", chandler.NewHeartbeatHandler("kiteclient-heartbeat", 2*time.Second, 1*time.Second, clientm))
	pipeline.RegisteHandler("kiteclient-accept", chandler.NewAcceptHandler("kiteclient-accept", listen))
	pipeline.RegisteHandler("kiteclient-remoting", pipe.NewRemotingHandler("kiteclient-remoting", clientm, flowControl))

	manager := &KiteClientManager{
		ga:            rclient.NewGroupAuth(groupId, secretKey),
		kiteClients:   make(map[string][]*kiteClient, 10),
		topics:        make([]string, 0, 10),
		pipeline:      pipeline,
		clientManager: clientm,
		flowControl:   flowControl}
	manager.zkManager = binding.NewZKManager(zkAddr, manager)

	return manager
}

//启动
func (self *KiteClientManager) Start() {

	hostname, _ := os.Hostname()
	//推送本机到
	err := self.zkManager.PublishTopics(self.topics, self.ga.GroupId, hostname)
	if nil != err {
		log.Fatalf("KiteClientManager|PublishTopics|FAIL|%s|%s\n", err, self.topics)
	} else {
		log.Printf("KiteClientManager|PublishTopics|SUCC|%s\n", self.topics)
	}

outter:
	for _, b := range self.binds {
		for _, t := range self.topics {
			if t == b.Topic {
				continue outter
			}
		}
		self.topics = append(self.topics, b.Topic)
	}

	for _, topic := range self.topics {

		hosts, err := self.zkManager.GetQServerAndWatch(topic)
		if nil != err {
			log.Fatalf("KiteClientManager|GetQServerAndWatch|FAIL|%s|%s\n", err, topic)
		} else {
			log.Printf("KiteClientManager|GetQServerAndWatch|SUCC|%s|%s\n", topic, hosts)
		}
		self.onQServerChanged(topic, hosts)
	}

	if len(self.kiteClients) <= 0 {
		log.Fatalf("KiteClientManager|Start|NO VALID KITESERVER|%s\n", self.topics)
	}

	if len(self.binds) > 0 {
		//订阅关系推送，并拉取QServer
		err = self.zkManager.PublishBindings(self.ga.GroupId, self.binds)
		if nil != err {
			log.Fatalf("KiteClientManager|PublishBindings|FAIL|%s|%s\n", err, self.binds)
		}
	}
	self.flowControl.Start()
}

//创建物理连接
func dial(hostport string) (*net.TCPConn, error) {
	//连接
	remoteAddr, err_r := net.ResolveTCPAddr("tcp4", hostport)
	if nil != err_r {
		log.Printf("KiteClientManager|RECONNECT|RESOLVE ADDR |FAIL|remote:%s\n", err_r)
		return nil, err_r
	}
	conn, err := net.DialTCP("tcp4", nil, remoteAddr)
	if nil != err {
		log.Printf("KiteClientManager|RECONNECT|%s|FAIL|%s\n", hostport, err)
		return nil, err
	}

	return conn, nil
}

func (self *KiteClientManager) NodeChange(path string, eventType binding.ZkEvent, children []string) {
	// @todo关闭或者新增相应的pub/sub connections
	//如果是订阅关系变更则处理
	if strings.HasPrefix(path, binding.KITEQ_SERVER) {
		//获取topic
		split := strings.Split(path, "/")
		if len(split) < 4 {
			//不合法的订阅璐姐
			log.Printf("KiteClientManager|ChildWatcher|INVALID SERVER PATH |%s|%t\n", path, children)
			return
		}
		//获取topic
		topic := split[3]
		//不是当前服务可以处理的topic则直接丢地啊哦
		if sort.SearchStrings(self.topics, topic) == len(self.topics) {
			log.Printf("BindExchanger|ChildWatcher|REFUSE SERVER PATH |%s|%t\n", path, children)
			return
		}
		self.onQServerChanged(topic, children)
	}
}

//当触发QServer地址发生变更
func (self *KiteClientManager) onQServerChanged(topic string, hosts []string) {
	self.lock.Lock()
	defer self.lock.Unlock()

	//重建一下topic下的kiteclient
	clients := make([]*kiteClient, 0, 10)
	for _, host := range hosts {
		//如果能查到remoteClient 则直接复用
		remoteClient := self.clientManager.FindRemoteClient(host)
		if nil == remoteClient {
			//这里就新建一个remote客户端连接
			conn, err := dial(host)
			if nil != err {
				log.Printf("KiteClientManager|onQServerChanged|Create REMOTE CLIENT|FAIL|%s|%s\n", err, host)
				continue
			}
			remoteClient = rclient.NewRemotingClient(conn,
				func(rc *rclient.RemotingClient, packet []byte) {
					self.flowControl.DispatcherFlow.Incr(1)
					event := pipe.NewPacketEvent(rc, packet)
					err := self.pipeline.FireWork(event)
					if nil != err {
						log.Printf("KiteClientManager|onPacketRecieve|FAIL|%s|%t\n", err, packet)
					}
				})
			remoteClient.Start()
			auth, err := handshake(self.ga, remoteClient)
			if !auth || nil != err {
				remoteClient.Shutdown()
				log.Printf("KiteClientManager|onQServerChanged|HANDSHAKE|FAIL|%s|%s\n", err, auth)
				continue
			}
			self.clientManager.Auth(self.ga, remoteClient)
		}

		//创建kiteClient
		kiteClient := newKitClient(remoteClient.RemoteAddr(), self.pipeline)
		clients = append(clients, kiteClient)
		log.Printf("KiteClientManager|onQServerChanged|newKitClient|SUCC|%s\n", host)
	}

	//替换掉线的server
	old, ok := self.kiteClients[topic]
	self.kiteClients[topic] = clients
	if ok {

		del := make([]string, 0, 2)
	outter:
		for _, o := range old {
			for _, c := range clients {
				if c.hostport == o.hostport {
					continue outter
				}
				del = append(del, c.hostport)
			}
		}

		if len(del) > 0 {
			self.clientManager.DeleteClients(del...)
		}
	}
}

func (self *KiteClientManager) DataChange(path string, binds []*binding.Binding) {
	//IGNORE
}

func (self *KiteClientManager) SetPublishTopics(topics []string) {
	self.topics = append(self.topics, topics...)
}

func (self *KiteClientManager) SetBindings(bindings []*binding.Binding) {
	for _, b := range bindings {
		b.GroupId = self.ga.GroupId
	}
	self.binds = bindings

}

func (self *KiteClientManager) SendMessage(topic string, msg interface{}) error {
	self.lock.RLock()
	defer self.lock.RUnlock()
	clients, ok := self.kiteClients[topic]
	if !ok {
		log.Println("KiteClientManager|SendMessage|FAIL|NO Remote Client|%s\n", msg)
		return errors.New("NO KITE CLIENT !")
	}

	if len(clients) <= 0 {
		return errors.New("NO KITE CLIENT !")
	}

	c := clients[rand.Intn(len(clients))]
	return c.sendMessage(msg)

}

func (self *KiteClientManager) Destory() {
	self.zkManager.Close()
}
