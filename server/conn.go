package server

import (
	"bufio"
	"context"
	"github.com/brodyxchen/vsock/client"
	"github.com/brodyxchen/vsock/constant"
	"github.com/brodyxchen/vsock/log"
	"github.com/brodyxchen/vsock/models"
	"github.com/brodyxchen/vsock/socket"
	"net"
	"runtime"
	"time"
)

type Conn struct {
	Name       string
	server     *Server
	remoteAddr string

	rwc       net.Conn
	bufReader *bufio.Reader
	bufWriter *bufio.Writer
}

func (c *Conn) Read(p []byte) (n int, err error) {
	return c.rwc.Read(p)
}

func (c *Conn) Write(p []byte) (n int, err error) {
	return c.rwc.Write(p)
}

func (c *Conn) handleServe(ctx context.Context, header *models.Header, body []byte) ([]byte, bool) {
	handler := c.server.getHandler(header.Code)
	if handler == nil {
		return nil, false
	}

	rspBytes := handler(header.Code, body)
	return rspBytes, true
}

// Serve a new connection.
func (c *Conn) serve(ctx context.Context) {
	defer c.Close()

	c.remoteAddr = c.rwc.RemoteAddr().String()

	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Errorf("http: panic serving %v: %v\n%s", c.remoteAddr, err, buf)
		}
	}()

	c.bufReader = getBufReader(c)
	c.bufWriter = getBufWriter(c)

	if c.server.ReadTimeout == 0 {
		_ = c.rwc.SetReadDeadline(time.Time{})
	}
	if c.server.WriteTimeout == 0 {
		_ = c.rwc.SetWriteDeadline(time.Time{})
	}

	waitOk := time.Time{}

	waitNext := func() bool {
		// 阻塞等待 下一份数据
		if wait := c.server.idleTimeout(); wait != 0 {
			_ = c.rwc.SetReadDeadline(time.Now().Add(wait))
			if _, err := c.bufReader.Peek(models.HeaderSize); err != nil {
				return false
			}
			waitOk = time.Now()
			log.Debug("Peek new request bytes")

			_ = c.rwc.SetReadDeadline(time.Time{})
			return true
		}
		return false
	}

	for {
		now := time.Now()

		// 设置底层conn read超时
		if c.server.ReadTimeout != 0 {
			_ = c.rwc.SetReadDeadline(now.Add(c.server.ReadTimeout))
		}

		waitGap := time.Since(waitOk)

		//todo 第一次进来(拨号)，没有Peek，此时可能读取异常
		// 1. read tcp 127.0.0.1:7070->127.0.0.1:64863: i/o timeout，    可能是对手client， 一直没发送数据？？？？
		// 2. io.EOF													可能是对手client， 关闭了conn？？？？
		header, body, err := socket.ReadSocketTest(c.Name, waitGap, ctx, c.bufReader)
		if err != nil {
			return
		}
		log.Debugf("readSocket : %+v\n", header)

		// 设置底层conn write超时
		if c.server.WriteTimeout != 0 {
			_ = c.rwc.SetWriteDeadline(time.Now().Add(c.server.WriteTimeout))
		}
		// handle
		rspBytes, ok := c.handleServe(ctx, header, body)
		if !ok {
			err = c.responseError(ctx, client.CodeInvalidAction, "invalid action")
			if err != nil {
				return
			}
			if !waitNext() {
				return
			}
			continue
		}

		// 响应rsp
		header.Code = 0
		header.Length = uint16(len(rspBytes))
		err = socket.WriteSocket(ctx, c.bufWriter, header, rspBytes)
		if err != nil {
			return
		}

		if !c.server.doKeepAlives() {
			return
		}
		if !waitNext() {
			return
		}
	}
}

func (c *Conn) responseError(ctx context.Context, code uint16, msg string) error {
	log.Debug("responseError : ", msg)

	header := &models.Header{
		Magic:   constant.DefaultMagic,
		Version: constant.DefaultVersion,
		Code:    code,
		Length:  0,
	}
	body := []byte(msg)
	header.Length = uint16(len(body))

	return socket.WriteSocket(ctx, c.bufWriter, header, body)
}

func (c *Conn) Close() {
	_ = c.rwc.Close()

	putBufReader(c.bufReader)
	putBufWriter(c.bufWriter)
}