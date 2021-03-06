package handler

import (
	"kiteq/binding"
	. "kiteq/pipe"
	"kiteq/protocol"
	"kiteq/store"
	// "log"
)

//----------------持久化的handler
type DeliverPreHandler struct {
	BaseForwardHandler
	kitestore store.IKiteStore
	exchanger *binding.BindExchanger
}

//------创建deliverpre
func NewDeliverPreHandler(name string, kitestore store.IKiteStore,
	exchanger *binding.BindExchanger) *DeliverPreHandler {
	phandler := &DeliverPreHandler{}
	phandler.BaseForwardHandler = NewBaseForwardHandler(name, phandler)
	phandler.kitestore = kitestore
	phandler.exchanger = exchanger
	return phandler
}

func (self *DeliverPreHandler) TypeAssert(event IEvent) bool {
	_, ok := self.cast(event)
	return ok
}

func (self *DeliverPreHandler) cast(event IEvent) (val *deliverEvent, ok bool) {
	val, ok = event.(*deliverEvent)
	return
}

func (self *DeliverPreHandler) Process(ctx *DefaultPipelineContext, event IEvent) error {

	pevent, ok := self.cast(event)
	if !ok {
		return ERROR_INVALID_EVENT_TYPE
	}

	//查询消息
	entity := self.kitestore.Query(pevent.messageId)
	data := protocol.MarshalMessage(entity.Header, entity.MsgType, entity.GetBody())

	//创建不同的packet
	switch entity.MsgType {
	case protocol.CMD_BYTES_MESSAGE:
		pevent.packet = protocol.NewPacket(protocol.CMD_BYTES_MESSAGE, data)
	case protocol.CMD_STRING_MESSAGE:
		pevent.packet = protocol.NewPacket(protocol.CMD_STRING_MESSAGE, data)
	}

	//填充订阅分组
	self.fillGroupIds(pevent, entity)
	self.fillDeliverExt(pevent, entity)
	ctx.SendForward(pevent)
	return nil

}

//填充订阅分组
func (self *DeliverPreHandler) fillGroupIds(pevent *deliverEvent, entity *store.MessageEntity) {
	binds := self.exchanger.FindBinds(entity.Header.GetTopic(), entity.Header.GetMessageType(), func(b *binding.Binding) bool {
		//过滤掉已经投递成功的分组
		// log.Printf("DeliverPreHandler|fillGroupIds|Filter Bind |%s|\n", b)
		return false
	})

	hashGroups := make(map[string]*string, 10)
	//按groupid归并
	for _, bind := range binds {
		hashGroups[bind.GroupId] = nil
	}

	//加入投递失败的分组
	for _, fg := range entity.FailGroups {
		hashGroups[fg] = nil
	}

	//去除掉已经投递成功的分组
	for _, sg := range entity.SuccGroups {
		delete(hashGroups, sg)
	}

	//合并本次需要投递的分组
	groupIds := make([]string, 0, 10)
	for k, _ := range hashGroups {
		groupIds = append(groupIds, k)
	}

	// //如果没有可用的分组则直接跳过
	// if len(groupIds) <= 0 {
	// 	log.Printf("DeliverPreHandler|Process|NO GROUPID TO DELIVERY |%s|%s|%s|%s\n", pevent.messageId, pevent.topic, pevent.messageType, binds)
	// } else {
	// 	log.Printf("DeliverPreHandler|Process|GROUPIDS TO DELIVERY |%s|%s|%s,%s\n", pevent.messageId, pevent.topic, pevent.messageType, groupIds)
	// }
	pevent.deliverGroups = groupIds
}

//填充投递的额外信息
func (self *DeliverPreHandler) fillDeliverExt(pevent *deliverEvent, entity *store.MessageEntity) {
	pevent.messageId = entity.Header.GetMessageId()
	pevent.topic = entity.Header.GetTopic()
	pevent.messageType = entity.Header.GetMessageType()
	pevent.expiredTime = entity.Header.GetExpiredTime()
	pevent.succGroups = entity.SuccGroups
	pevent.deliverLimit = entity.DeliverLimit
	pevent.deliverCount = entity.DeliverCount
}
