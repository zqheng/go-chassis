package highway

import (
	"bufio"
	"context"
	highwayclient "github.com/ServiceComb/go-chassis/client/highway"
	"github.com/ServiceComb/go-chassis/client/highway/pb"
	"github.com/ServiceComb/go-chassis/core/common"
	"github.com/ServiceComb/go-chassis/core/handler"
	"github.com/ServiceComb/go-chassis/core/invocation"
	"github.com/ServiceComb/go-chassis/core/lager"
	"github.com/ServiceComb/go-chassis/core/provider"
	"github.com/ServiceComb/go-chassis/third_party/forked/go-micro/metadata"
	"net"
	"sync"
)

type ConnectionMgr struct {
	conns map[string]*HighwayConnection
	count int
	sync.RWMutex
}

func NewConnectMgr() *ConnectionMgr {
	tmp := new(ConnectionMgr)
	tmp.count = 0
	tmp.conns = make(map[string]*HighwayConnection)
	return tmp
}

func (this *ConnectionMgr) CreateConn(baseConn net.Conn, handlerChain string) *HighwayConnection {
	this.Lock()
	defer this.Unlock()
	conn := NewHighwayConnection(baseConn, handlerChain, this)
	this.conns[conn.GetRemoteAddr()] = conn
	this.count++
	return conn
}

func (this *ConnectionMgr) DeleteConn(addr string) {
	this.Lock()
	defer this.Unlock()
	delete(this.conns, addr)
	this.count--
}

func (this *ConnectionMgr) DeactiveAllConn() {
	for _, conn := range this.conns {
		conn.Close()
	}
}

//Highway connection
type HighwayConnection struct {
	remoteAddr   string
	handlerChain string
	baseConn     net.Conn
	mtx          *sync.Mutex
	closed       bool
	connMgr      *ConnectionMgr
}

//Create service connection
func NewHighwayConnection(conn net.Conn, handlerChain string, connMgr *ConnectionMgr) *HighwayConnection {
	return &HighwayConnection{(conn.(*net.TCPConn)).RemoteAddr().String(), handlerChain, conn, &sync.Mutex{}, false, connMgr}
}

//open service connection
func (this *HighwayConnection) Open() {
	go this.msgRecvLoop()
}

//Get remote addr
func (this *HighwayConnection) GetRemoteAddr() string {
	return this.remoteAddr
}

//Close connection
func (svrConn *HighwayConnection) Close() {
	svrConn.mtx.Lock()
	defer svrConn.mtx.Unlock()
	if svrConn.closed {
		return
	}
	svrConn.connMgr.DeleteConn(svrConn.remoteAddr)
	svrConn.closed = true
	svrConn.baseConn.Close()
}

//handshake
func (svrConn *HighwayConnection) Hello() error {
	var err error
	rdBuf := bufio.NewReaderSize(svrConn.baseConn, highwayclient.DefaultReadBufferSize)
	protoObj := &highwayclient.HighWayProtocalObject{}
	protoObj.DeSerializeFrame(rdBuf)
	if err != nil {
		return err
	}

	req := &highwayclient.HighwayRequest{}
	req.Arg = &highway.LoginRequest{}
	err = protoObj.DeSerializeReq(req)
	if err != nil {
		return err
	}

	if loginRequest, ok := req.Arg.(*highway.LoginRequest); ok {
		if loginRequest.UseProtobufMapCodec == true {
			wBuf := bufio.NewWriterSize(svrConn.baseConn, highwayclient.DefaultWriteBufferSize)
			protoObj.SerializelLoginRsp(req.MsgID, wBuf)
			err := wBuf.Flush()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (svrConn *HighwayConnection) msgRecvLoop() {
	if svrConn.Hello() != nil {
		//Handshake failed , Close the conn
		svrConn.Close()
		return
	}
	rdBuf := bufio.NewReaderSize(svrConn.baseConn, highwayclient.DefaultReadBufferSize)
	for {
		protoObj := &highwayclient.HighWayProtocalObject{}
		err := protoObj.DeSerializeFrame(rdBuf)
		if err != nil {
			lager.Logger.Errorf(err, "DeSerializeFrame failed.")
			break
		}
		go svrConn.hanleFrame(protoObj)
	}
	svrConn.Close()
}

//send error msg
func (this *HighwayConnection) writeError(req *highwayclient.HighwayRequest, err error) {
	if req.TwoWay {
		protoObj := &highwayclient.HighWayProtocalObject{}
		wBuf := bufio.NewWriterSize(this.baseConn, highwayclient.DefaultWriteBufferSize)
		rsp := &highwayclient.HighwayRespond{}
		rsp.Result = nil
		rsp.MsgID = req.MsgID
		rsp.Err = err.Error()
		rsp.Status = highwayclient.ServerError
		protoObj.SerializeRsp(rsp, wBuf)
		errSnd := wBuf.Flush()
		if errSnd != nil {
			this.Close()
			lager.Logger.Errorf(errSnd, "writeError failed.")
		}
	}
}

func (svrConn *HighwayConnection) hanleFrame(protoObj *highwayclient.HighWayProtocalObject) error {
	var err error
	req := &highwayclient.HighwayRequest{}
	err = protoObj.DeSerializeReq(req)
	if err != nil {
		lager.Logger.Errorf(err, "DeSerializeReq failed.")
		return err
	}

	i := &invocation.Invocation{}
	i.Args = req.Arg
	i.MicroServiceName = req.SvcName
	i.SchemaID = req.Schema
	i.OperationID = req.MethodName
	if req.Attachments != nil {
		i.SourceMicroService = req.Attachments[common.HeaderSourceName]
	}
	i.Ctx = metadata.NewContext(context.Background(), req.Attachments)
	i.Protocol = common.ProtocolHighway
	c, err := handler.GetChain(common.Provider, svrConn.handlerChain)
	if err != nil {
		lager.Logger.Errorf(err, "Handler chain init err")
		svrConn.writeError(req, err)
	}

	c.Next(i, func(ir *invocation.InvocationResponse) error {
		if ir.Err != nil {
			svrConn.writeError(req, ir.Err)
			return ir.Err
		}
		p, err := provider.GetProvider(i.MicroServiceName)
		if err != nil {
			svrConn.writeError(req, err)
			return err
		}
		r, err := p.Invoke(i)
		if err != nil {
			svrConn.writeError(req, err)
			return err
		}
		if req.TwoWay {
			wBuf := bufio.NewWriterSize(svrConn.baseConn, highwayclient.DefaultWriteBufferSize)
			rsp := &highwayclient.HighwayRespond{}
			rsp.Result = r
			rsp.Status = highwayclient.Ok
			rsp.MsgID = req.MsgID
			protoObj.SerializeRsp(rsp, wBuf)
			err = wBuf.Flush()
			if err != nil {
				lager.Logger.Errorf(err, "Send Respond failed.")
				svrConn.Close()
				return err
			}
		}
		return err

	})

	return nil
}