package requests

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"net/http"

	"github.com/gospider007/gtls"
	"github.com/gospider007/http3"
	"github.com/gospider007/net/http2"
	"github.com/gospider007/tools"
	"github.com/quic-go/quic-go"
	utls "github.com/refraction-networking/utls"
)

type reqTask struct {
	ctx           context.Context
	cnl           context.CancelFunc
	req           *http.Request
	res           *http.Response
	emptyPool     chan struct{}
	err           error
	orderHeaders  []string
	orderHeaders2 []string
}

func (obj *reqTask) inPool() bool {
	return obj.err == nil && obj.res != nil && obj.res.StatusCode != 101 && !strings.Contains(obj.res.Header.Get("Content-Type"), "text/event-stream")
}
func (obj *reqTask) suppertRetry() bool {
	if obj.req.Body == nil {
		return true
	} else if body, ok := obj.req.Body.(*requestBody); ok {
		if body.Seek(0, io.SeekStart); !body.readed {
			return true
		}
	}
	return false
}
func getKey(ctxData *reqCtxData, req *http.Request) (key string) {
	var proxyUser string
	if ctxData.proxy != nil {
		proxyUser = ctxData.proxy.User.String()
	}
	return fmt.Sprintf("%s@%s@%s", proxyUser, getAddr(ctxData.proxy), getAddr(req.URL))
}

type roundTripper struct {
	ctx        context.Context
	cnl        context.CancelFunc
	connPools  *connPools
	dialer     *DialClient
	tlsConfig  *tls.Config
	utlsConfig *utls.Config
	getProxy   func(ctx context.Context, url *url.URL) (string, error)
}

func newRoundTripper(preCtx context.Context, option ClientOption) *roundTripper {
	if preCtx == nil {
		preCtx = context.TODO()
	}
	ctx, cnl := context.WithCancel(preCtx)
	dialClient := NewDail(DialOption{
		DialTimeout: option.DialTimeout,
		Dns:         option.Dns,
		KeepAlive:   option.KeepAlive,
		LocalAddr:   option.LocalAddr,
		AddrType:    option.AddrType,
		GetAddrType: option.GetAddrType,
	})
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		// SessionTicketKey:   [32]byte{},
		ClientSessionCache: tls.NewLRUClientSessionCache(0),
	}
	utlsConfig := &utls.Config{
		InsecureSkipVerify:     true,
		InsecureSkipTimeVerify: true,
		// SessionTicketKey:                   [32]byte{},
		ClientSessionCache:                 utls.NewLRUClientSessionCache(0),
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}
	return &roundTripper{
		tlsConfig:  tlsConfig,
		utlsConfig: utlsConfig,
		ctx:        ctx,
		cnl:        cnl,
		dialer:     dialClient,
		getProxy:   option.GetProxy,
		connPools:  newConnPools(),
	}
}
func (obj *roundTripper) newConnPool(conn *connecotr, key string) *connPool {
	pool := new(connPool)
	pool.connKey = key
	pool.forceCtx, pool.forceCnl = context.WithCancelCause(obj.ctx)
	pool.safeCtx, pool.safeCnl = context.WithCancelCause(pool.forceCtx)
	pool.tasks = make(chan *reqTask)
	pool.connPools = obj.connPools
	pool.total.Add(1)
	go pool.rwMain(conn)
	return pool
}
func (obj *roundTripper) putConnPool(key string, conn *connecotr) {
	conn.inPool = true
	if conn.h2RawConn == nil && conn.h3RawConn == nil {
		go conn.read()
	}
	pool := obj.connPools.get(key)
	if pool != nil {
		pool.total.Add(1)
		go pool.rwMain(conn)
	} else {
		obj.connPools.set(key, obj.newConnPool(conn, key))
	}
}
func (obj *roundTripper) tlsConfigClone(ctxData *reqCtxData) *tls.Config {
	if ctxData.tlsConfig != nil {
		return ctxData.tlsConfig.Clone()
	}
	return obj.tlsConfig.Clone()
}
func (obj *roundTripper) utlsConfigClone(ctxData *reqCtxData) *utls.Config {
	if ctxData.utlsConfig != nil {
		return ctxData.utlsConfig.Clone()
	}
	return obj.utlsConfig.Clone()
}
func (obj *roundTripper) newConnecotr(netConn net.Conn) *connecotr {
	conne := new(connecotr)
	conne.withCancel(obj.ctx, obj.ctx)
	conne.rawConn = netConn
	return conne
}

func (obj *roundTripper) http3Dial(ctxData *reqCtxData, req *http.Request) (conn *connecotr, err error) {
	var netConn quic.EarlyConnection
	tlsConfig := obj.tlsConfigClone(ctxData)
	tlsConfig.NextProtos = []string{http3.NextProtoH3}
	tlsConfig.ServerName = req.Host
	netConn, err = http3.DialEarly(req.Context(), getAddr(req.URL), tlsConfig, nil)
	if err != nil {
		return
	}
	conn = obj.newConnecotr(nil)
	conn.h3RawConn = http3.NewClient(netConn)
	return
}
func (obj *roundTripper) dial(ctxData *reqCtxData, req *http.Request) (conn *connecotr, err error) {
	if ctxData.h3 {
		return obj.http3Dial(ctxData, req)
	}
	var proxy *url.URL
	if !ctxData.disProxy {
		if proxy = cloneUrl(ctxData.proxy); proxy == nil && obj.getProxy != nil {
			proxyStr, err := obj.getProxy(req.Context(), proxy)
			if err != nil {
				return conn, err
			}
			if proxy, err = gtls.VerifyProxy(proxyStr); err != nil {
				return conn, err
			}
		}
	}
	netConn, err := obj.dialer.DialContextWithProxy(req.Context(), ctxData, "tcp", req.URL.Scheme, getAddr(req.URL), getHost(req), proxy, obj.tlsConfigClone(ctxData))
	if err != nil {
		return conn, err
	}
	var h2 bool
	if req.URL.Scheme == "https" {
		ctx, cnl := context.WithTimeout(req.Context(), ctxData.tlsHandshakeTimeout)
		defer cnl()
		if ctxData.ja3Spec.IsSet() {
			tlsConfig := obj.utlsConfigClone(ctxData)
			if ctxData.forceHttp1 {
				tlsConfig.NextProtos = []string{"http/1.1"}
			}
			tlsConn, err := obj.dialer.addJa3Tls(ctx, netConn, getHost(req), ctxData.isWs || ctxData.forceHttp1, ctxData.ja3Spec, tlsConfig)
			if err != nil {
				return conn, tools.WrapError(err, "add ja3 tls error")
			}
			h2 = tlsConn.ConnectionState().NegotiatedProtocol == "h2"
			netConn = tlsConn
		} else {
			tlsConn, err := obj.dialer.addTls(ctx, netConn, getHost(req), ctxData.isWs || ctxData.forceHttp1, obj.tlsConfigClone(ctxData))
			if err != nil {
				return conn, tools.WrapError(err, "add tls error")
			}
			h2 = tlsConn.ConnectionState().NegotiatedProtocol == "h2"
			netConn = tlsConn
		}
	}
	conne := obj.newConnecotr(netConn)
	if proxy != nil {
		conne.proxy = proxy.String()
	}
	if h2 {
		if conne.h2RawConn, err = http2.NewClientConn(func() {
			conne.safeCnl(errors.New("http2 client close"))
		}, netConn, ctxData.h2Ja3Spec); err != nil {
			return conne, err
		}
	} else {
		conne.r, conne.w = bufio.NewReader(conne), bufio.NewWriter(conne)
	}
	return conne, err
}
func (obj *roundTripper) setGetProxy(getProxy func(ctx context.Context, url *url.URL) (string, error)) {
	obj.getProxy = getProxy
}

func (obj *roundTripper) poolRoundTrip(ctxData *reqCtxData, pool *connPool, task *reqTask) (isTry bool) {
	task.ctx, task.cnl = context.WithTimeout(task.req.Context(), ctxData.responseHeaderTimeout)
	select {
	case pool.tasks <- task:
		select {
		case <-task.emptyPool:
			return true
		case <-task.ctx.Done():
			if task.err == nil && task.res == nil {
				task.err = context.Cause(task.ctx)
			}
			return false
		}
	default:
		return true
	}
}
func (obj *roundTripper) connRoundTripMain(ctxData *reqCtxData, task *reqTask, key string) {
	for {
		if !obj.connRoundTrip(ctxData, task, key) {
			return
		}
	}
}
func (obj *roundTripper) connRoundTrip(ctxData *reqCtxData, task *reqTask, key string) (retry bool) {
	ctxData.isNewConn = true
	conn, err := obj.dial(ctxData, task.req)
	if err != nil {
		task.err = err
		return
	}
	task.ctx, task.cnl = context.WithTimeout(task.req.Context(), ctxData.responseHeaderTimeout)
	retry = conn.taskMain(task, false)
	if retry || task.err != nil {
		return retry
	}
	if task.inPool() && !ctxData.disAlive {
		obj.putConnPool(key, conn)
	}
	return retry
}

func (obj *roundTripper) closeConns() {
	obj.connPools.iter(func(key string, pool *connPool) bool {
		pool.safeClose()
		obj.connPools.del(key)
		return true
	})
}
func (obj *roundTripper) forceCloseConns() {
	obj.connPools.iter(func(key string, pool *connPool) bool {
		pool.forceClose()
		obj.connPools.del(key)
		return true
	})
}
func (obj *roundTripper) newReqTask(req *http.Request, ctxData *reqCtxData) *reqTask {
	if ctxData.responseHeaderTimeout == 0 {
		ctxData.responseHeaderTimeout = time.Second * 300
	}
	task := new(reqTask)
	task.req = req
	task.emptyPool = make(chan struct{})
	task.orderHeaders = ctxData.orderHeaders
	if ctxData.h2Ja3Spec.OrderHeaders != nil {
		task.orderHeaders2 = ctxData.h2Ja3Spec.OrderHeaders
	} else {
		task.orderHeaders2 = ctxData.orderHeaders
	}
	return task
}
func (obj *roundTripper) RoundTrip(req *http.Request) (response *http.Response, err error) {
	ctxData := GetReqCtxData(req.Context())
	if ctxData.requestCallBack != nil {
		if err = ctxData.requestCallBack(req.Context(), req, nil); err != nil {
			if err == http.ErrUseLastResponse {
				if req.Response == nil {
					return nil, errors.New("errUseLastResponse response is nil")
				} else {
					return req.Response, nil
				}
			}
			return nil, err
		}
	}
	key := getKey(ctxData, req) //pool key
	task := obj.newReqTask(req, ctxData)
	if ctxData.disAlive {
		obj.connRoundTripMain(ctxData, task, key)
	} else {
		for {
			pool := obj.connPools.get(key)
			if pool == nil {
				obj.connRoundTripMain(ctxData, task, key)
				break
			}
			if !obj.poolRoundTrip(ctxData, pool, task) {
				break
			}
		}
	}
	if task.err == nil && ctxData.requestCallBack != nil {
		if err = ctxData.requestCallBack(task.req.Context(), task.req, task.res); err != nil {
			task.err = err
		}
	}
	return task.res, task.err
}
