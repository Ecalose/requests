package requests

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"net/http"

	"github.com/gospider007/gtls"
	"github.com/gospider007/http2"
	"github.com/gospider007/http3"
	"github.com/gospider007/tools"
	"github.com/quic-go/quic-go"
	uquic "github.com/refraction-networking/uquic"
)

type reqTask struct {
	option    *RequestOption
	ctx       context.Context
	cnl       context.CancelFunc
	req       *http.Request
	res       *http.Response
	emptyPool chan struct{}
	err       error
}

func (obj *reqTask) suppertRetry() bool {
	if obj.req.Body == nil {
		return true
	} else if body, ok := obj.req.Body.(io.Seeker); ok {
		if i, err := body.Seek(0, io.SeekStart); i == 0 && err == nil {
			return true
		}
	}
	return false
}
func getKey(option *RequestOption, req *http.Request) (key string) {
	return fmt.Sprintf("%s@%s", getAddr(option.proxy), getAddr(req.URL))
}

type roundTripper struct {
	ctx       context.Context
	cnl       context.CancelFunc
	connPools *connPools
	dialer    *Dialer
}

func newRoundTripper(preCtx context.Context) *roundTripper {
	if preCtx == nil {
		preCtx = context.TODO()
	}
	ctx, cnl := context.WithCancel(preCtx)
	return &roundTripper{
		ctx:       ctx,
		cnl:       cnl,
		dialer:    &Dialer{},
		connPools: newConnPools(),
	}
}
func (obj *roundTripper) newConnPool(done chan struct{}, conn *connecotr, key string) *connPool {
	pool := new(connPool)
	pool.connKey = key
	pool.forceCtx, pool.forceCnl = context.WithCancelCause(obj.ctx)
	pool.safeCtx, pool.safeCnl = context.WithCancelCause(pool.forceCtx)
	pool.tasks = make(chan *reqTask)

	pool.connPools = obj.connPools
	pool.total.Add(1)
	go pool.rwMain(done, conn)
	return pool
}
func (obj *roundTripper) putConnPool(key string, conn *connecotr) {
	pool := obj.connPools.get(key)
	done := make(chan struct{})
	if pool != nil {
		pool.total.Add(1)
		go pool.rwMain(done, conn)
	} else {
		obj.connPools.set(key, obj.newConnPool(done, conn, key))
	}
	<-done
}
func (obj *roundTripper) newConnecotr() *connecotr {
	conne := new(connecotr)
	conne.withCancel(obj.ctx, obj.ctx)
	return conne
}

func (obj *roundTripper) http3Dial(ctx context.Context, option *RequestOption, remtoeAddress Address, proxyAddress ...Address) (udpConn net.PacketConn, err error) {
	if len(proxyAddress) > 0 {
		if proxyAddress[len(proxyAddress)-1].Scheme != "socks5" {
			err = errors.New("http3 last proxy must socks5 proxy")
			return
		}
		// udpConn, err = obj.dialer.Socks5UdpProxy(ctx, option, remtoeAddress, proxyAddress...)
		udpConn, _, err = obj.dialer.DialProxyContext(ctx, option, "tcp", option.TlsConfig.Clone(), append(proxyAddress, remtoeAddress)...)
	} else {
		udpConn, err = net.ListenUDP("udp", nil)
	}
	return
}
func (obj *roundTripper) ghttp3Dial(ctx context.Context, option *RequestOption, remoteAddress Address, proxyAddress ...Address) (conn *connecotr, err error) {
	udpConn, err := obj.http3Dial(ctx, option, remoteAddress, proxyAddress...)
	if err != nil {
		return nil, err
	}
	tlsConfig := option.TlsConfig.Clone()
	tlsConfig.NextProtos = []string{http3.NextProtoH3}
	tlsConfig.ServerName = remoteAddress.Host
	if remoteAddress.IP == nil {
		remoteAddress.IP, err = obj.dialer.loadHost(ctx, remoteAddress.Name, option)
		if err != nil {
			return nil, err
		}
	}
	netConn, err := quic.DialEarly(ctx, udpConn, &net.UDPAddr{IP: remoteAddress.IP, Port: remoteAddress.Port}, tlsConfig, nil)
	conn = obj.newConnecotr()
	conn.Conn = http3.NewClient(netConn, func() {
		conn.forceCnl(errors.New("http3 client close"))
	})
	return
}

func (obj *roundTripper) uhttp3Dial(ctx context.Context, option *RequestOption, remoteAddress Address, proxyAddress ...Address) (conn *connecotr, err error) {
	udpConn, err := obj.http3Dial(ctx, option, remoteAddress, proxyAddress...)
	if err != nil {
		return nil, err
	}
	tlsConfig := option.UtlsConfig.Clone()
	tlsConfig.NextProtos = []string{http3.NextProtoH3}
	tlsConfig.ServerName = remoteAddress.Host
	if remoteAddress.IP == nil {
		remoteAddress.IP, err = obj.dialer.loadHost(ctx, remoteAddress.Name, option)
		if err != nil {
			return nil, err
		}
	}
	netConn, err := uquic.DialEarly(ctx, udpConn, remoteAddress, tlsConfig, nil)
	conn = obj.newConnecotr()
	conn.Conn = http3.NewUClient(netConn, func() {
		conn.forceCnl(errors.New("http3 client close"))
	})
	return
}

func (obj *roundTripper) dial(option *RequestOption, req *http.Request) (conn *connecotr, err error) {
	proxys, err := obj.initProxys(option, req)
	if err != nil {
		return nil, err
	}
	remoteAddress, err := GetAddressWithUrl(req.URL)
	if err != nil {
		return nil, err
	}
	if option.H3 {
		if option.Ja3Spec.IsSet() {
			return obj.uhttp3Dial(req.Context(), option, remoteAddress, proxys...)
		} else {
			return obj.ghttp3Dial(req.Context(), option, remoteAddress, proxys...)
		}
	}
	var netConn net.Conn
	if len(proxys) > 0 {
		_, netConn, err = obj.dialer.DialProxyContext(req.Context(), option, "tcp", option.TlsConfig.Clone(), append(proxys, remoteAddress)...)
	} else {
		var remoteAddress Address
		remoteAddress, err = GetAddressWithUrl(req.URL)
		if err != nil {
			return nil, err
		}
		netConn, err = obj.dialer.DialContext(req.Context(), option, "tcp", remoteAddress)
	}
	defer func() {
		if err != nil && netConn != nil {
			netConn.Close()
		}
	}()
	if err != nil {
		return nil, err
	}
	var h2 bool
	if req.URL.Scheme == "https" {
		netConn, h2, err = obj.dialAddTls(option, req, netConn)
		if option.Logger != nil {
			option.Logger(Log{
				Id:   option.requestId,
				Time: time.Now(),
				Type: LogType_TLSHandshake,
				Msg:  fmt.Sprintf("host:%s,  h2:%t", getHost(req), h2),
			})
		}
		if err != nil {
			return nil, err
		}
	}
	conne := obj.newConnecotr()
	conne.proxys = proxys
	conne.c = netConn
	err = obj.dialConnecotr(option, req, conne, h2)
	if err != nil {
		return nil, err
	}
	return conne, err
}
func (obj *roundTripper) dialConnecotr(option *RequestOption, req *http.Request, conne *connecotr, h2 bool) (err error) {
	if h2 {
		if option.H2Ja3Spec.OrderHeaders != nil {
			option.OrderHeaders = option.H2Ja3Spec.OrderHeaders
		}
		if conne.Conn, err = http2.NewClientConn(req.Context(), conne.c, option.H2Ja3Spec, func() {
			conne.forceCnl(errors.New("http2 client close"))
		}); err != nil {
			return err
		}
	} else {
		conne.Conn = newConn(conne.forceCtx, conne.c, func() {
			conne.forceCnl(errors.New("http1 client close"))
		})
	}
	return err
}
func (obj *roundTripper) dialAddTls(option *RequestOption, req *http.Request, netConn net.Conn) (net.Conn, bool, error) {
	ctx, cnl := context.WithTimeout(req.Context(), option.TlsHandshakeTimeout)
	defer cnl()
	if option.Ja3Spec.IsSet() {
		if tlsConn, err := obj.dialer.addJa3Tls(ctx, netConn, getHost(req), option.ForceHttp1, option.Ja3Spec, option.UtlsConfig.Clone()); err != nil {
			return tlsConn, false, tools.WrapError(err, "add ja3 tls error")
		} else {
			return tlsConn, tlsConn.ConnectionState().NegotiatedProtocol == "h2", nil
		}
	} else {
		if tlsConn, err := obj.dialer.addTls(ctx, netConn, getHost(req), option.ForceHttp1, option.TlsConfig.Clone()); err != nil {
			return tlsConn, false, tools.WrapError(err, "add tls error")
		} else {
			return tlsConn, tlsConn.ConnectionState().NegotiatedProtocol == "h2", nil
		}
	}
}
func (obj *roundTripper) initProxys(option *RequestOption, req *http.Request) ([]Address, error) {
	var proxys []Address
	if option.DisProxy {
		return nil, nil
	}
	if option.proxy != nil {
		proxyAddress, err := GetAddressWithUrl(option.proxy)
		if err != nil {
			return nil, err
		}
		proxys = []Address{proxyAddress}
	}
	if len(proxys) == 0 && len(option.proxys) > 0 {
		proxys = make([]Address, len(option.proxys))
		for i, proxy := range option.proxys {
			proxyAddress, err := GetAddressWithUrl(proxy)
			if err != nil {
				return nil, err
			}
			proxys[i] = proxyAddress
		}
	}
	if len(proxys) == 0 && option.GetProxy != nil {
		proxyStr, err := option.GetProxy(req.Context(), req.URL)
		if err != nil {
			return proxys, err
		}
		if proxyStr != "" {
			proxy, err := gtls.VerifyProxy(proxyStr)
			if err != nil {
				return proxys, err
			}
			proxyAddress, err := GetAddressWithUrl(proxy)
			if err != nil {
				return nil, err
			}
			proxys = []Address{proxyAddress}
		}
	}
	if len(proxys) == 0 && option.GetProxys != nil {
		proxyStrs, err := option.GetProxys(req.Context(), req.URL)
		if err != nil {
			return proxys, err
		}
		if l := len(proxyStrs); l > 0 {
			proxys = make([]Address, l)
			for i, proxyStr := range proxyStrs {
				proxy, err := gtls.VerifyProxy(proxyStr)
				if err != nil {
					return proxys, err
				}
				proxyAddress, err := GetAddressWithUrl(proxy)
				if err != nil {
					return nil, err
				}
				proxys[i] = proxyAddress
			}
		}
	}
	return proxys, nil
}

func (obj *roundTripper) poolRoundTrip(option *RequestOption, pool *connPool, task *reqTask, key string) (isOk bool, err error) {
	task.ctx, task.cnl = context.WithTimeout(task.req.Context(), option.ResponseHeaderTimeout)
	select {
	case pool.tasks <- task:
		select {
		case <-task.emptyPool:
			return false, nil
		case <-task.ctx.Done():
			if task.err == nil && task.res == nil {
				task.err = context.Cause(task.ctx)
			}
			return true, task.err
		}
	default:
		return obj.createPool(option, task, key)
	}
}

func (obj *roundTripper) createPool(option *RequestOption, task *reqTask, key string) (isOk bool, err error) {
	option.isNewConn = true
	conn, err := obj.dial(option, task.req)
	if err != nil {
		if task.option.ErrCallBack != nil {
			if err2 := task.option.ErrCallBack(task.req.Context(), task.option, nil, err); err2 != nil {
				return true, err2
			}
		}
		return false, err
	}
	obj.putConnPool(key, conn)
	return false, nil
}

func (obj *roundTripper) closeConns() {
	for key, pool := range obj.connPools.Range() {
		pool.safeClose()
		obj.connPools.del(key)
	}
}
func (obj *roundTripper) forceCloseConns() {
	for key, pool := range obj.connPools.Range() {
		pool.forceClose()
		obj.connPools.del(key)
	}
}
func (obj *roundTripper) newReqTask(req *http.Request, option *RequestOption) *reqTask {
	if option.ResponseHeaderTimeout == 0 {
		option.ResponseHeaderTimeout = time.Second * 300
	}
	task := new(reqTask)
	task.req = req
	task.option = option
	task.emptyPool = make(chan struct{})
	return task
}
func (obj *roundTripper) RoundTrip(req *http.Request) (response *http.Response, err error) {
	option := GetRequestOption(req.Context())
	if option.RequestCallBack != nil {
		if err = option.RequestCallBack(req.Context(), req, nil); err != nil {
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
	key := getKey(option, req) //pool key
	task := obj.newReqTask(req, option)
	maxRetry := 10
	var errNum int
	var isOk bool
	for {
		select {
		case <-req.Context().Done():
			return nil, context.Cause(req.Context())
		default:
		}
		if errNum >= maxRetry {
			task.err = fmt.Errorf("roundTrip retry %d times", maxRetry)
			break
		}
		pool := obj.connPools.get(key)
		if pool == nil {
			isOk, err = obj.createPool(option, task, key)
		} else {
			isOk, err = obj.poolRoundTrip(option, pool, task, key)
		}
		if isOk {
			if err != nil {
				task.err = err
			}
			break
		}
		if err != nil {
			errNum++
		}
	}
	if task.err == nil && option.RequestCallBack != nil {
		if err = option.RequestCallBack(task.req.Context(), task.req, task.res); err != nil {
			task.err = err
		}
	}
	return task.res, task.err
}
