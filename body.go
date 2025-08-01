package requests

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"

	"github.com/gospider007/gson"
	"github.com/gospider007/tools"
)

type OrderData struct {
	data []struct {
		key string
		val any
	}
}

func NewOrderData() *OrderData {
	return &OrderData{
		data: []struct {
			key string
			val any
		}{},
	}
}

func (obj *OrderData) Add(key string, val any) {
	obj.data = append(obj.data, struct {
		key string
		val any
	}{key: key, val: val})
}
func (obj *OrderData) Keys() []string {
	keys := make([]string, len(obj.data))
	for i, value := range obj.data {
		keys[i] = value.key
	}
	return keys
}
func (obj *OrderData) ReorderWithKeys(key ...string) {
	if len(key) == 0 {
		return
	}
	for i, k := range key {
		key[i] = textproto.CanonicalMIMEHeaderKey(k)
	}
	sort.SliceStable(obj.data, func(x, y int) bool {
		xIndex := -1
		yIndex := -1
		for i, k := range key {
			if k == obj.data[x].key {
				xIndex = i
			}
			if k == obj.data[y].key {
				yIndex = i
			}
		}
		if xIndex == -1 {
			return false
		}
		if yIndex == -1 {
			return true
		}
		return xIndex < yIndex
	})
}

type orderT struct {
	key string
	val any
}

func (obj orderT) Key() string {
	return obj.key
}
func (obj orderT) Val() any {
	return obj.val
}

func (obj *OrderData) Data() []interface {
	Key() string
	Val() any
} {
	if obj == nil {
		return nil
	}
	keys := make([]interface {
		Key() string
		Val() any
	}, len(obj.data))
	for i, value := range obj.data {
		keys[i] = orderT{
			key: value.key,
			val: value.val,
		}
	}
	return keys
}

func formWrite(writer *multipart.Writer, key string, val any) (err error) {
	switch value := val.(type) {
	case File:
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, escapeQuotes(key), escapeQuotes(value.FileName)))
		if value.ContentType == "" {
			switch content := value.Content.(type) {
			case []byte:
				h.Set("Content-Type", http.DetectContentType(content))
			case string:
				h.Set("Content-Type", http.DetectContentType(tools.StringToBytes(content)))
			case io.Reader:
				h.Set("Content-Type", "application/octet-stream")
			default:
				con, err := gson.Encode(content)
				if err != nil {
					return err
				}
				h.Set("Content-Type", http.DetectContentType(con))
			}
		} else {
			h.Set("Content-Type", value.ContentType)
		}
		var wp io.Writer
		if wp, err = writer.CreatePart(h); err != nil {
			return
		}
		switch content := value.Content.(type) {
		case []byte:
			_, err = wp.Write(content)
		case string:
			_, err = wp.Write(tools.StringToBytes(content))
		case io.Reader:
			_, err = tools.Copy(wp, content)
		default:
			con, err := gson.Encode(content)
			if err != nil {
				return err
			}
			_, err = wp.Write(con)
			if err != nil {
				return err
			}
		}
	case []byte:
		err = writer.WriteField(key, tools.BytesToString(value))
	case string:
		err = writer.WriteField(key, value)
	default:
		con, err := gson.Encode(val)
		if err != nil {
			return err
		}
		err = writer.WriteField(key, tools.BytesToString(con))
		if err != nil {
			return err
		}
	}
	return
}
func (obj *OrderData) isformPip() bool {
	if len(obj.data) == 0 {
		return false
	}
	for _, value := range obj.data {
		if file, ok := value.val.(File); ok {
			if _, ok := file.Content.(io.Reader); ok {
				return true
			}
		}
	}
	return false
}
func (obj *OrderData) formWriteMain(writer *multipart.Writer) (err error) {
	for _, value := range obj.data {
		if err = formWrite(writer, value.key, value.val); err != nil {
			return
		}
	}
	return writer.Close()
}

func paramsWrite(buf *bytes.Buffer, key string, val any) error {
	if buf.Len() > 0 {
		buf.WriteByte('&')
	}
	buf.WriteString(url.QueryEscape(key))
	buf.WriteByte('=')
	var err error
	switch value := val.(type) {
	case []byte:
		_, err = buf.Write(value)
	case string:
		_, err = buf.WriteString(value)
	default:
		v, err2 := gson.Encode(val)
		if err2 != nil {
			return err2
		}
		_, err = buf.Write(v)
	}
	return err
}
func (obj *OrderData) MarshalJSON() ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	err := buf.WriteByte('{')
	if err != nil {
		return nil, err
	}
	for i, value := range obj.data {
		if i > 0 {
			if err = buf.WriteByte(','); err != nil {
				return nil, err
			}
		}
		if _, err = buf.WriteString(`"` + value.key + `":`); err != nil {
			return nil, err
		}
		val, err := gson.Encode(value.val)
		if err != nil {
			return nil, err
		}
		if _, err = buf.Write(val); err != nil {
			return nil, err
		}
	}
	if err = buf.WriteByte('}'); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (obj *RequestOption) newBody(val any) (io.Reader, *OrderData, error) {
	switch value := val.(type) {
	case *OrderData:
		return nil, value, nil
	case io.Reader:
		obj.readOne = true
		return value, nil, nil
	case string:
		return bytes.NewReader(tools.StringToBytes(value)), nil, nil
	case []byte:
		return bytes.NewReader(value), nil, nil
	case map[string]any:
		orderMap := NewOrderData()
		for key, val := range value {
			orderMap.Add(key, val)
		}
		return nil, orderMap, nil
	default:
		jsonData, err := gson.Decode(val)
		if err != nil {
			return nil, nil, errors.New("invalid body type")
		}
		orderMap := NewOrderData()
		for kk, vv := range jsonData.Map() {
			orderMap.Add(kk, vv.Value())
		}
		return nil, orderMap, nil
	}
}

func (obj *OrderData) parseParams() *bytes.Buffer {
	buf := bytes.NewBuffer(nil)
	for _, value := range obj.data {
		paramsWrite(buf, value.key, value.val)
	}
	return buf
}
func (obj *OrderData) parseForm(ctx context.Context, boundary string) (io.Reader, bool, error) {
	if len(obj.data) == 0 {
		return nil, false, nil
	}
	if obj.isformPip() {
		pr, pw := io.Pipe()
		writer := multipart.NewWriter(pw)
		go func() {
			stop := context.AfterFunc(ctx, func() {
				pw.CloseWithError(ctx.Err())
			})
			defer stop()
			pw.CloseWithError(obj.formWriteMain(writer))
		}()
		return pr, true, nil
	}
	body := bytes.NewBuffer(nil)
	writer := multipart.NewWriter(body)
	writer.SetBoundary(boundary)
	err := obj.formWriteMain(writer)
	if err != nil {
		return nil, false, err
	}
	return bytes.NewReader(body.Bytes()), false, err
}
func (obj *OrderData) parseData() io.Reader {
	val := obj.parseParams().Bytes()
	if val == nil {
		return nil
	}
	return bytes.NewReader(val)
}
func (obj *OrderData) parseJson() (io.Reader, error) {
	con, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(con), nil
}
func (obj *OrderData) parseText() (io.Reader, error) {
	con, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(con), nil
}

// Upload files with form-data,
type File struct {
	Content     any
	FileName    string
	ContentType string
}

func randomBoundary() (string, string) {
	var buf [30]byte
	io.ReadFull(rand.Reader, buf[:])
	boundary := fmt.Sprintf("%x", buf[:])
	if strings.ContainsAny(boundary, `()<>@,;:\"/[]?= `) {
		boundary = `"` + boundary + `"`
	}
	return "multipart/form-data; boundary=" + boundary, boundary
}

func (obj *RequestOption) initBody(ctx context.Context) (io.Reader, error) {
	if obj.Body != nil {
		body, orderData, err := obj.newBody(obj.Body)
		if err != nil {
			return nil, err
		}
		if body != nil {
			return body, nil
		}
		con, err := orderData.MarshalJSON()
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(con), nil
	} else if obj.Form != nil {
		var boundary string
		if obj.ContentType == "" {
			obj.ContentType, boundary = randomBoundary()
		}
		body, orderData, err := obj.newBody(obj.Form)
		if err != nil {
			return nil, err
		}
		if body != nil {
			return body, nil
		}
		body, once, err := orderData.parseForm(ctx, boundary)
		if err != nil {
			return nil, err
		}
		obj.readOne = once
		return body, err
	} else if obj.Data != nil {
		if obj.ContentType == "" {
			obj.ContentType = "application/x-www-form-urlencoded"
		}
		body, orderData, err := obj.newBody(obj.Data)
		if err != nil {
			return nil, err
		}
		if body != nil {
			return body, nil
		}
		return orderData.parseData(), nil
	} else if obj.Json != nil {
		if obj.ContentType == "" {
			obj.ContentType = "application/json"
		}
		body, orderData, err := obj.newBody(obj.Json)
		if err != nil {
			return nil, err
		}
		if body != nil {
			return body, nil
		}
		return orderData.parseJson()
	} else if obj.Text != nil {
		if obj.ContentType == "" {
			obj.ContentType = "text/plain"
		}
		body, orderData, err := obj.newBody(obj.Text)
		if err != nil {
			return nil, err
		}
		if body != nil {
			return body, nil
		}
		return orderData.parseText()
	} else {
		return nil, nil
	}
}
func (obj *RequestOption) initParams() (*url.URL, error) {
	baseUrl := cloneUrl(obj.Url)
	if obj.Params == nil {
		return baseUrl, nil
	}
	body, dataData, err := obj.newBody(obj.Params)
	if err != nil {
		return nil, err
	}
	var query string
	if body != nil {
		paramsBytes, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		query = tools.BytesToString(paramsBytes)
	} else {
		query = dataData.parseParams().String()
	}
	if query == "" {
		return baseUrl, nil
	}
	pquery := baseUrl.Query().Encode()
	if pquery == "" {
		baseUrl.RawQuery = query
	} else {
		baseUrl.RawQuery = pquery + "&" + query
	}
	return baseUrl, nil
}
