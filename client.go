package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"log"
	"sync"
	"time"
)

type BiliWsClientConfig struct {
	Name     string
	Host     string
	Port     int64
	RoomId   int64
	AuthBody string
}

type BiliWsClient struct {
	*websocket.Conn
	conf       *BiliWsClientConfig
	dispather  *protoDispather
	decoder    *DecodeManager
	bufPool    sync.Pool // TODO
	sequenceId int32
	closeFlag  chan struct{}
	authed     bool
	retryCnt   int64
	disconnecd bool
	msgBuf     chan *Proto
}

// NewBiliWsClient 新建一个ws请求头
func NewBiliWsClient(conf *BiliWsClientConfig) *BiliWsClient {
	if conf == nil {
		panic("[BiliWsClient | NewBiliWsClient] conf == nil")
	}
	c := &BiliWsClient{
		conf:      conf,
		dispather: newMessageDispather(),
		closeFlag: make(chan struct{}),
		decoder:   NewDecodeManager(),
		msgBuf:    make(chan *Proto, 1024),
	}
	var err error
	wsAddr := fmt.Sprintf("ws://%s:%d/sub", c.conf.Host, c.conf.Port)
	//wss连接
	c.Conn, _, err = websocket.DefaultDialer.Dial(wsAddr, nil)
	if err != nil {
		log.Fatal("[BiliWsClient | NewBiliWsClient] connect err")
		return nil
	}
	log.Println("[BiliWsClient | NewBiliWsClient] connect success")

	//认证结果插入
	c.registerProtoHandler(OP_AUTH_REPLY, c.authResp)
	//心跳包插入
	c.registerProtoHandler(OP_HEARTBEAT_REPLY, c.heartBeatResp)
	//消息插入
	c.registerProtoHandler(OP_SEND_SMS_REPLY, c.msgResp)

	//发送认证
	err = c.sendAuth(c.conf.AuthBody)
	if err != nil {
		log.Fatal("[BiliWsClient | NewBiliWsClient] sendAuth err:", err)
		return nil
	}
	return c
}

// Run 运行协程
func (c *BiliWsClient) Run() {
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.doReadLoop()
	}()
	go func() {
		defer wg.Done()
		c.doEventLoop()
	}()
	wg.Done()
}

// 发送认证
func (c *BiliWsClient) sendAuth(authBody string) (err error) {
	p := &Proto{
		Operation: OP_AUTH,
		Body:      []byte(authBody),
	}
	return c.sendMsg(p)
}

// 心跳头部处理
func (c *BiliWsClient) sendHeartBeat() {
	if !c.authed {
		return
	}
	msg := &Proto{}
	msg.Operation = OP_HEARTBEAT
	msg.SequenceId = c.sequenceId
	c.sequenceId++
	err := c.sendMsg(msg)
	if err != nil {
		log.Fatal("[BiliWsClient | sendHeartBeat] err", err)
		return
	}
	log.Println("[BiliWsClient | sendHeartBeat] seq:", msg.SequenceId)
}

func (c *BiliWsClient) registerProtoHandler(cmd int32, logic protoLogic) {
	c.dispather.register(cmd, logic)
}

func (c *BiliWsClient) Close() {
}

// 认证头部处理
func (c *BiliWsClient) sendMsg(msg *Proto) (err error) {
	dataBuff := &bytes.Buffer{}
	packLen := int32(RawHeaderSize + len(msg.Body))
	msg.HeaderLength = RawHeaderSize
	binary.Write(dataBuff, binary.BigEndian, packLen)
	binary.Write(dataBuff, binary.BigEndian, int16(RawHeaderSize))
	binary.Write(dataBuff, binary.BigEndian, msg.Version)
	binary.Write(dataBuff, binary.BigEndian, msg.Operation)
	binary.Write(dataBuff, binary.BigEndian, msg.SequenceId)
	binary.Write(dataBuff, binary.BigEndian, msg.Body)
	err = c.Conn.WriteMessage(websocket.BinaryMessage, dataBuff.Bytes())
	if err != nil {
		err = errors.Wrapf(err, "[BiliWsClient | SendMsg] WriteMessage err")
		return
	}
	return
}

// 读取返回消息
func (c *BiliWsClient) readMsg() {
	retProto := &Proto{}
	//读出返回消息存放buff中
	_, buf, err := c.Conn.ReadMessage()
	if err != nil {
		retProto.ErrMsg = errors.Wrapf(err, "[BiliWsClient | ReadMsg] conn err")
		return
	}
	//判断buff里面的长度是否小于16
	if len(buf) < RawHeaderSize {
		retProto.ErrMsg = errors.Wrapf(err, "[BiliWsClient | ReadMsg] buf:%d less", len(buf))
		return
	}
	//此处可参考: https://open-live.bilibili.com/document/doc&tool/api/websocket.html#_1-%E5%8F%91%E9%80%81auth%E5%8C%85
	//读整包长度
	retProto.PacketLength = int32(binary.BigEndian.Uint32(buf[PackOffset:HeaderOffset]))
	//读包头长度
	retProto.HeaderLength = int16(binary.BigEndian.Uint16(buf[HeaderOffset:VerOffset]))
	//读版本信息
	retProto.Version = int16(binary.BigEndian.Uint16(buf[VerOffset:OperationOffset]))
	//读消息类型
	retProto.Operation = int32(binary.BigEndian.Uint32(buf[OperationOffset:SeqIdOffset]))
	//保留字段
	retProto.SequenceId = int32(binary.BigEndian.Uint32(buf[SeqIdOffset:]))
	if retProto.PacketLength < 0 || retProto.PacketLength > MaxPackSize {
		retProto.ErrMsg = errors.Wrapf(err, "[BiliWsClient | ReadMsg] PacketLength:%d err", retProto.PacketLength)
		return
	}
	if retProto.HeaderLength != RawHeaderSize {
		retProto.ErrMsg = errors.Wrapf(err, "[BiliWsClient | ReadMsg] HeaderLength:%d err", retProto.PacketLength)
		return
	}
	//判断是否为压缩包
	if bodyLen := int(retProto.PacketLength - int32(retProto.HeaderLength)); bodyLen > 0 {
		retProto.Body = buf[retProto.HeaderLength:retProto.PacketLength]
	} else {
		retProto.ErrMsg = errors.Wrapf(err, "[BiliWsClient | ReadMsg] BodyLength:%d err", bodyLen)
		return
	}
	//根据版本信息解码信息
	retProto.BodyMuti, err = c.decoder.Decode(int64(retProto.Version), retProto.Body)
	if len(retProto.BodyMuti) > 0 {
		retProto.Body = retProto.BodyMuti[0]
	}
	c.msgBuf <- retProto
}

// 主计时器,查看解码好的数据
func (c *BiliWsClient) doEventLoop() {
	ticker := time.NewTicker(time.Second * 5)
	for {
		select {
		case p := <-c.msgBuf:
			if p == nil {
				log.Fatal("[BiliWsClient | ReadMsg] p == nil")
				continue
			}
			if p.ErrMsg != nil {
				log.Fatal("[BiliWsClient | ReadMsg] err:", p.ErrMsg)
				continue
			}
			err := c.dispather.do(p)
			if err != nil {
				log.Fatal("[BiliWsClient | ReadMsg] dispather err:", err)
				continue
			}
		case <-c.closeFlag:
			goto exit
		case <-ticker.C:
			c.sendHeartBeat()
		}
	}
exit:
	c.Close()
}

// 等待服务器返回消息
func (c *BiliWsClient) doReadLoop() {
	for {
		c.readMsg()
	}
}

// 认证返回信息
func (c *BiliWsClient) authResp(msg *Proto) (err error) {
	resp := &AuthRespParam{}
	err = json.Unmarshal(msg.Body, resp)
	if err != nil {
		err = errors.Wrapf(err, "[BiliWsClient | AuthResp] Unmarshal err")
		return
	}
	if resp.Code != 0 {
		err = fmt.Errorf("[BiliWsClient | AuthResp] code:%d", resp.Code)
		return
	}
	c.authed = true
	log.Println("[BiliWsClient | AuthResp] auth success")
	return
}

// 心跳包
func (c *BiliWsClient) heartBeatResp(msg *Proto) (err error) {
	log.Println("[BiliWsClient | HeartBeatResp] recv HeartBeat resp", msg.Body)
	return
}

// MsgResp 可以这里做回调
func (c *BiliWsClient) msgResp(msg *Proto) (err error) {
	for index, cmd := range msg.BodyMuti {
		log.Printf("[BiliWsClient | HeartBeatResp] recv MsgResp index:%d ver:%d cmd:%s", index, msg.Version, string(cmd))
	}
	return
}

type protoLogic func(p *Proto) (err error)

type protoDispather struct {
	dispather map[int32]protoLogic
}

func newMessageDispather() *protoDispather {
	return &protoDispather{
		dispather: map[int32]protoLogic{},
	}
}

func (m *protoDispather) register(Op int32, f protoLogic) {
	if m.dispather[Op] != nil {
		panic(fmt.Sprintf("[MessageDispather | Register] Op:%d repeated", Op))
	}
	m.dispather[Op] = f
}

func (m *protoDispather) do(p *Proto) (err error) {
	f, exist := m.dispather[p.Operation]
	if exist {
		fmt.Println("proto:", p.Version)
		err = f(p)
		if err != nil {
			errors.Wrapf(err, "[MessageDispather | Do] process err")
		}
		return
	}
	return fmt.Errorf("[MessageDispather | Do] Op:%d not found", p.Operation)
}
