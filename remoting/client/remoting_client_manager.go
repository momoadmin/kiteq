package client

import (
	"log"
	"sync"
	"time"
)

//群组授权信息
type GroupAuth struct {
	GroupId, SecretKey string
	authtime           int64
}

func NewGroupAuth(groupId, secretKey string) *GroupAuth {
	return &GroupAuth{SecretKey: secretKey, GroupId: groupId, authtime: time.Now().Unix()}
}

//远程client管理器
type ClientManager struct {
	reconnectManager *ReconnectManager
	groupAuth        map[string] /*host:port*/ *GroupAuth
	groupClients     map[string] /*groupId*/ []*RemotingClient
	allClients       map[string] /*host:port*/ *RemotingClient
	lock             sync.RWMutex
}

func NewClientManager(reconnectManager *ReconnectManager) *ClientManager {

	return &ClientManager{
		groupAuth:        make(map[string]*GroupAuth, 10),
		groupClients:     make(map[string][]*RemotingClient, 50),
		allClients:       make(map[string]*RemotingClient, 100),
		reconnectManager: reconnectManager}
}

//验证是否授权
func (self *ClientManager) Validate(remoteClient *RemotingClient) bool {
	self.lock.RLock()
	defer self.lock.RUnlock()
	_, auth := self.groupAuth[remoteClient.RemoteAddr()]
	return auth
}

func (self *ClientManager) Auth(auth *GroupAuth, remoteClient *RemotingClient) bool {
	self.lock.Lock()
	defer self.lock.Unlock()

	cs, ok := self.groupClients[auth.GroupId]
	if !ok {
		cs = make([]*RemotingClient, 0, 50)
	}
	//创建remotingClient
	self.groupClients[auth.GroupId] = append(cs, remoteClient)
	self.allClients[remoteClient.RemoteAddr()] = remoteClient
	self.groupAuth[remoteClient.RemoteAddr()] = auth
	return true
}

func (self *ClientManager) ClientsClone() map[string]*RemotingClient {
	self.lock.RLock()
	defer self.lock.RUnlock()

	clone := make(map[string]*RemotingClient, len(self.allClients))
	for k, v := range self.allClients {
		clone[k] = v
	}
	return clone
}

func (self *ClientManager) DeleteClients(hostports ...string) {
	self.lock.Lock()
	defer self.lock.Unlock()
	for _, hostport := range hostports {
		self.reconnectFailHook(hostport)
	}
}

func (self *ClientManager) reconnectFailHook(hostport string) {
	_, ok := self.groupAuth[hostport]
	if ok {
		delete(self.groupAuth, hostport)
		clients, ok := self.groupClients[hostport]
		if ok {
			for i, c := range clients {
				//如果是当前链接
				if c.RemoteAddr() == hostport {
					c.Shutdown()
					self.groupClients[hostport] = append(clients[:i], clients[i+1:]...)
					break
				}
			}
			delete(self.allClients, hostport)
			log.Printf("ClientManager|reconnectFailHook|Remove Client|%s\n", hostport)
		}
	}
}

func (self *ClientManager) SubmitReconnect(c *RemotingClient) {
	ga, ok := self.groupAuth[c.RemoteAddr()]
	if ok {
		//如果重连则提交重连任务
		if self.reconnectManager.allowReconnect {
			self.reconnectManager.submit(newReconnectTasK(c, ga, func(addr string) {
				//重连任务失败完成后的hook,直接移除该机器
				self.lock.Lock()
				defer self.lock.Unlock()
				self.reconnectFailHook(addr)
			}))
		} else {
			//不需要重连的直接删除掉连接
			self.reconnectFailHook(c.RemoteAddr())
		}
	}
}

//查找remotingclient
func (self *ClientManager) FindRemoteClient(hostport string) *RemotingClient {
	self.lock.RLock()
	defer self.lock.RUnlock()
	// log.Printf("ClientManager|FindRemoteClient|%s|%s\n", hostport, self.allClients)
	rclient, ok := self.allClients[hostport]
	if ok && rclient.IsClosed() {
		self.SubmitReconnect(rclient)
	}
	return rclient
}

//查找匹配的groupids
func (self *ClientManager) FindRemoteClients(groupIds []string, filter func(groupId string, rc *RemotingClient) bool) map[string][]*RemotingClient {
	self.lock.RLock()
	defer self.lock.RUnlock()
	clients := make(map[string][]*RemotingClient, 10)
	for _, gid := range groupIds {
		if len(self.groupClients[gid]) <= 0 {
			continue
		}
		//按groupId来获取remoteclient
		gclient, ok := clients[gid]
		if !ok {
			gclient = make([]*RemotingClient, 0, 10)
		}

		for _, c := range self.groupClients[gid] {

			if c.IsClosed() {
				//提交到重连
				self.SubmitReconnect(c)
				continue
			}
			//如果当前client处于非关闭状态并且没有过滤则入选
			if !filter(gid, c) {
				gclient = append(gclient, c)
			}

		}
		clients[gid] = gclient
	}
	// log.Printf("Find clients result |%s|%s\n", clients, self.groupClients)
	return clients
}

func (self *ClientManager) Shutdown() {
	for _, c := range self.allClients {
		c.Shutdown()
	}
}
