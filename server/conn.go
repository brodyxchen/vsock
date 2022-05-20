package server

import (
	"bufio"
	"context"
	"fmt"
	"github.com/brodyxchen/vsock/constant"
	"github.com/brodyxchen/vsock/errors"
	"github.com/brodyxchen/vsock/log"
	"github.com/brodyxchen/vsock/models"
	"github.com/brodyxchen/vsock/protocols"
	"github.com/brodyxchen/vsock/socket"
	"google.golang.org/protobuf/proto"
	"io"
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

func (c *Conn) handleServe(ctx context.Context, body []byte) ([]byte, error) {
	wrap := func(bytes []byte, err error) []byte {
		var rsp *protocols.Response
		if err != nil {
			rsp = &protocols.Response{
				Code: protocols.StatusErr,
				Rsp:  nil,
				Err:  err.Error(),
			}
		} else {
			rsp = &protocols.Response{
				Code: protocols.StatusOK,
				Rsp:  bytes,
				Err:  "",
			}
		}
		rspBytes, err := proto.Marshal(rsp)
		if err != nil {
			panic(err)
		}
		return rspBytes
	}

	var request protocols.Request
	err := proto.Unmarshal(body, &request)
	if err != nil {
		return nil, errors.StatusInvalidRequest
	}

	handler := c.server.getHandler(request.Path)
	if handler == nil {
		return nil, errors.StatusInvalidPath
	}

	rsp := wrap(handler(request.Req))

	return rsp, nil
}

// Serve a new connection.
func (c *Conn) serve(ctx context.Context) {
	defer c.server.connsHist.Dec(1)

	closeErr := errors.New("serve default close")
	defer func() {
		c.Close(closeErr)
	}()

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

	waitNext := func() error { // 阻塞等待 下一份数据
		for {
			wait := c.server.idleTimeout()

			if wait != 0 {
				_ = c.rwc.SetReadDeadline(time.Now().Add(wait))
			} else {
				_ = c.rwc.SetReadDeadline(time.Time{})
			}

			_, err := c.bufReader.Peek(1) //models.HeaderSize
			if err != nil {
				if err == io.EOF {
					continue
				}
				return errors.New("serve wait peek err : " + err.Error()) // io.EOF 代表对面关闭了???  or i/o timeout
			}
			_ = c.rwc.SetReadDeadline(time.Time{})
			return nil
		}

	}

	for {
		if err := waitNext(); err != nil {
			closeErr = err
			return
		}

		// 设置底层conn read超时
		now := time.Now()
		if c.server.ReadTimeout != 0 {
			_ = c.rwc.SetReadDeadline(now.Add(c.server.ReadTimeout))
		}

		readNow := time.Now()
		header, body, broken, err := socket.ReadSocket(ctx, c.bufReader)
		c.server.readHist.Update(time.Since(readNow).Milliseconds())

		if err != nil {
			if broken {
				closeErr = err
				return
			}
			continue
		}

		// 设置底层conn write超时
		if c.server.WriteTimeout != 0 {
			_ = c.rwc.SetWriteDeadline(time.Now().Add(c.server.WriteTimeout))
		}
		// handle
		rspBytes, status := c.handleServe(ctx, body)

		writeNow := time.Now()
		if status != nil {
			broken, err := c.responseStatus(ctx, status.(*errors.Status))
			c.server.writeHist.Update(time.Since(writeNow).Milliseconds())
			if err != nil && broken {
				closeErr = err
				return
			}
		} else {
			broken, err := c.responseSuccess(ctx, header, rspBytes)
			c.server.writeHist.Update(time.Since(writeNow).Milliseconds())
			if err != nil && broken {
				closeErr = err
				return
			}
		}

		// keepAlive
		if !c.server.doKeepAlives() {
			closeErr = errors.ErrNoKeepAlive
			return
		}
	}
}

func (c *Conn) responseSuccess(ctx context.Context, header *models.Header, rspBytes []byte) (bool, error) {
	header.Code = 0
	header.Length = uint16(len(rspBytes))
	return socket.WriteSocket(ctx, c.bufWriter, header, rspBytes)
}

func (c *Conn) responseStatus(ctx context.Context, status *errors.Status) (bool, error) {
	header := &models.Header{
		Magic:   constant.DefaultMagic,
		Version: constant.DefaultVersion,
		Code:    status.Code(),
		Length:  0,
	}
	body := []byte(status.Error())
	header.Length = uint16(len(body))

	return socket.WriteSocket(ctx, c.bufWriter, header, body)
}

func (c *Conn) Close(err error) {
	fmt.Println("conn.close() ", c.Name, err)
	_ = c.rwc.Close()

	putBufReader(c.bufReader)
	putBufWriter(c.bufWriter)
}
