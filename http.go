package requests

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/gospider007/tools"
	"golang.org/x/net/http/httpguts"
)

type reqReadWriteCtx struct {
	// req *http.Request
	// res          *http.Response
	// orderHeaders []interface {
	// 	Key() string
	// 	Val() any
	// }
	// err error

	writeCtx context.Context
	writeCnl context.CancelFunc

	readCtx context.Context
	readCnl context.CancelCauseFunc
}
type clientConn struct {
	err          error
	readWriteCtx *reqReadWriteCtx
	conn         net.Conn
	r            *bufio.Reader
	w            *bufio.Writer
	closeFunc    func(error)
	ctx          context.Context

	closeCtx context.Context
	closeCnl context.CancelCauseFunc
}

func newClientConn(ctx context.Context, con net.Conn, closeFunc func(error)) *clientConn {
	closeCtx, closeCnl := context.WithCancelCause(ctx)
	reader, writer := io.Pipe()
	c := &clientConn{
		closeCtx:  closeCtx,
		closeCnl:  closeCnl,
		ctx:       ctx,
		conn:      con,
		closeFunc: closeFunc,
		r:         bufio.NewReader(reader),
		w:         bufio.NewWriter(con),
	}
	go func() {
		_, err := io.Copy(writer, con)
		writer.CloseWithError(err)
		c.CloseWithError(err)
	}()
	return c
}

var errLastTaskRuning = errors.New("last task is running")

func (obj *clientConn) send(req *http.Request, orderHeaders []interface {
	Key() string
	Val() any
}) (res *http.Response, err error) {
	go obj.httpWrite(req, req.Header.Clone(), orderHeaders)
	res, err = http.ReadResponse(obj.r, req)
	if err == nil && res == nil {
		err = errors.New("response is nil")
	}
	if err != nil {
		obj.readWriteCtx.readCnl(nil)
		return
	}
	rawBody := res.Body
	pr, pw := io.Pipe()
	go func() {
		var readErr error
		defer func() {
			obj.readWriteCtx.readCnl(readErr)
		}()
		noBody := res.Body == nil || (res.ContentLength == -1 && len(res.TransferEncoding) == 0)
		if !noBody {
			_, readErr = io.Copy(pw, rawBody)
		}
		pw.CloseWithError(readErr)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			err = tools.WrapError(readErr, "failed to read response body")
		} else {
			readErr = nil
		}
		if readErr != nil {
			obj.CloseWithError(readErr)
		} else {
			select {
			case <-obj.readWriteCtx.writeCtx.Done():
				if res.StatusCode == 101 || strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
					select {
					case <-obj.ctx.Done():
						return
					case <-obj.closeCtx.Done():
						return
					}
				}
			default:
				obj.CloseWithError(tools.WrapError(errLastTaskRuning, "last task not write done with read done"))
				return
			}
		}
	}()
	res.Body = pr
	return
}

func (obj *clientConn) CloseCtx() context.Context {
	return obj.closeCtx
}

func (obj *clientConn) Close() error {
	return obj.CloseWithError(nil)
}
func (obj *clientConn) CloseWithError(err error) error {
	obj.closeCnl(err)
	if err == nil {
		obj.err = tools.WrapError(obj.err, "connecotr closeWithError close")
	} else {
		obj.err = tools.WrapError(err, "connecotr closeWithError close")
	}
	if obj.closeFunc != nil {
		obj.closeFunc(obj.err)
	}
	return obj.conn.Close()
}
func (obj *clientConn) initTask() {
	readCtx, readCnl := context.WithCancelCause(obj.closeCtx)
	writeCtx, writeCnl := context.WithCancel(obj.closeCtx)
	obj.readWriteCtx = &reqReadWriteCtx{
		readCtx:  readCtx,
		readCnl:  readCnl,
		writeCtx: writeCtx,
		writeCnl: writeCnl,
	}
}
func (obj *clientConn) DoRequest(req *http.Request, orderHeaders []interface {
	Key() string
	Val() any
}) (*http.Response, context.Context, error) {
	if obj.readWriteCtx != nil {
		select {
		case <-obj.readWriteCtx.writeCtx.Done():
		case <-obj.ctx.Done():
			return nil, nil, obj.ctx.Err()
		case <-obj.closeCtx.Done():
			return nil, nil, obj.closeCtx.Err()
		default:
			return nil, obj.readWriteCtx.readCtx, errLastTaskRuning
		}
		select {
		case <-obj.readWriteCtx.readCtx.Done():
		case <-obj.ctx.Done():
			return nil, nil, obj.ctx.Err()
		case <-obj.closeCtx.Done():
			return nil, nil, obj.closeCtx.Err()
		default:
			return nil, obj.readWriteCtx.readCtx, errLastTaskRuning
		}
	} else {
		select {
		case <-obj.ctx.Done():
			return nil, nil, obj.ctx.Err()
		case <-obj.closeCtx.Done():
			return nil, nil, obj.closeCtx.Err()
		default:
		}
	}
	obj.initTask()
	res, err := obj.send(req, orderHeaders)
	if err != nil {
		obj.CloseWithError(err)
		return nil, nil, err
	}
	return res, obj.readWriteCtx.readCtx, err
}

type websocketConn struct {
	r   io.Reader
	w   io.WriteCloser
	cnl context.CancelCauseFunc
}

func (obj *websocketConn) Read(p []byte) (n int, err error) {
	return obj.r.Read(p)
}
func (obj *websocketConn) Write(p []byte) (n int, err error) {
	// i, err := obj.w.Write(p)
	// log.Print(err, "  write error  ", i, p)
	// return i, err
	return obj.w.Write(p)
}
func (obj *websocketConn) Close() error {
	obj.cnl(nil)
	return obj.w.Close()
}

func (obj *clientConn) Stream() io.ReadWriteCloser {
	return &websocketConn{
		cnl: obj.closeCnl,
		r:   obj.r,
		w:   obj.conn,
	}
}
func (obj *clientConn) httpWrite(req *http.Request, rawHeaders http.Header, orderHeaders []interface {
	Key() string
	Val() any
}) {
	var err error
	defer func() {
		if err != nil {
			obj.CloseWithError(tools.WrapError(err, "failed to send request body"))
		}
		obj.readWriteCtx.writeCnl()
	}()
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	host, err = httpguts.PunycodeHostPort(host)
	if err != nil {
		return
	}
	host = removeZone(host)
	if rawHeaders.Get("Host") == "" {
		rawHeaders.Set("Host", host)
	}
	contentL, chunked := tools.GetContentLength(req)
	if contentL >= 0 {
		rawHeaders.Set("Content-Length", fmt.Sprint(contentL))
	} else if chunked {
		rawHeaders.Set("Transfer-Encoding", "chunked")
	}
	ruri := req.URL.RequestURI()
	if req.Method == "CONNECT" && req.URL.Path == "" {
		if req.URL.Opaque != "" {
			ruri = req.URL.Opaque
		} else {
			ruri = host
		}
	}
	if _, err = obj.w.WriteString(fmt.Sprintf("%s %s %s\r\n", req.Method, ruri, req.Proto)); err != nil {
		return
	}
	for _, kv := range tools.NewHeadersWithH1(orderHeaders, rawHeaders) {
		if _, err = obj.w.WriteString(fmt.Sprintf("%s: %s\r\n", kv[0], kv[1])); err != nil {
			return
		}
	}
	if _, err = obj.w.WriteString("\r\n"); err != nil {
		return
	}
	if req.Body == nil {
		err = obj.w.Flush()
		return
	}
	if chunked {
		chunkedWriter := newChunkedWriter(obj.w)
		if _, err = io.Copy(chunkedWriter, req.Body); err != nil {
			return
		}
		if err = chunkedWriter.Close(); err != nil {
			return
		}
	} else {
		if _, err = io.Copy(obj.w, req.Body); err != nil {
			return
		}
	}
	err = obj.w.Flush()
}

func newChunkedWriter(w *bufio.Writer) io.WriteCloser {
	return &chunkedWriter{w}
}

type chunkedWriter struct {
	w *bufio.Writer
}

func (cw *chunkedWriter) Write(data []byte) (n int, err error) {
	if len(data) == 0 {
		return 0, nil
	}
	if _, err = fmt.Fprintf(cw.w, "%x\r\n", len(data)); err != nil {
		return 0, err
	}
	if _, err = cw.w.Write(data); err != nil {
		return
	}
	if _, err = io.WriteString(cw.w, "\r\n"); err != nil {
		return
	}
	return len(data), cw.w.Flush()
}

func (cw *chunkedWriter) Close() error {
	_, err := io.WriteString(cw.w, "0\r\n\r\n")
	return err
}
