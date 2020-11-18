// Copyright 2012 Gary Burd
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package redis

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	_ ConnWithTimeout = (*activeConn)(nil)
	_ ConnWithTimeout = (*errorConn)(nil)
)

var nowFunc = time.Now // for testing

// ErrPoolExhausted is returned from a pool connection method (Do, Send,
// Receive, Flush, Err) when the maximum number of database connections in the
// pool has been reached.
var ErrPoolExhausted = errors.New("redigo: connection pool exhausted")

var (
	errPoolClosed = errors.New("redigo: connection pool closed")
	errConnClosed = errors.New("redigo: connection closed")
)

// Pool maintains a pool of connections. The application calls the Get method
// to get a connection from the pool and the connection's Close method to
// return the connection's resources to the pool.
//
// The following example shows how to use a pool in a web application. The
// application creates a pool at application startup and makes it available to
// request handlers using a package level variable. The pool configuration used
// here is an example, not a recommendation.
//
//  func newPool(addr string) *redis.Pool {
//    return &redis.Pool{
//      MaxIdle: 3,
//      IdleTimeout: 240 * time.Second,
//      // Dial or DialContext must be set. When both are set, DialContext takes precedence over Dial.
//      Dial: func () (redis.Conn, error) { return redis.Dial("tcp", addr) },
//    }
//  }
//
//  var (
//    pool *redis.Pool
//    redisServer = flag.String("redisServer", ":6379", "")
//  )
//
//  func main() {
//    flag.Parse()
//    pool = newPool(*redisServer)
//    ...
//  }
//
// A request handler gets a connection from the pool and closes the connection
// when the handler is done:
//
//  func serveHome(w http.ResponseWriter, r *http.Request) {
//      conn := pool.Get()
//      defer conn.Close()
//      ...
//  }
//
// Use the Dial function to authenticate connections with the AUTH command or
// select a database with the SELECT command:
//
//  pool := &redis.Pool{
//    // Other pool configuration not shown in this example.
//    Dial: func () (redis.Conn, error) {
//      c, err := redis.Dial("tcp", server)
//      if err != nil {
//        return nil, err
//      }
//      if _, err := c.Do("AUTH", password); err != nil {
//        c.Close()
//        return nil, err
//      }
//      if _, err := c.Do("SELECT", db); err != nil {
//        c.Close()
//        return nil, err
//      }
//      return c, nil
//    },
//  }
//
// Use the TestOnBorrow function to check the health of an idle connection
// before the connection is returned to the application. This example PINGs
// connections that have been idle more than a minute:
//
//  pool := &redis.Pool{
//    // Other pool configuration not shown in this example.
//    TestOnBorrow: func(c redis.Conn, t time.Time) error {
//      if time.Since(t) < time.Minute {
//        return nil
//      }
//      _, err := c.Do("PING")
//      return err
//    },
//  }
//
type Pool struct {
	// Dial is an application supplied function for creating and configuring a
	// connection.
	//
	// The connection returned from Dial must not be in a special state
	// (subscribed to pubsub channel, transaction started, ...).
	Dial func() (Conn, error)												// 连接Redis的函数

	// DialContext is an application supplied function for creating and configuring a
	// connection with the given context.
	//
	// The connection returned from Dial must not be in a special state
	// (subscribed to pubsub channel, transaction started, ...).
	DialContext func(ctx context.Context) (Conn, error)						// 连接Redis的函数，带context可以设置超时时间

	// TestOnBorrow is an optional application supplied function for checking
	// the health of an idle connection before the connection is used again by
	// the application. Argument t is the time that the connection was returned
	// to the pool. If the function returns an error, then the connection is
	// closed.
	TestOnBorrow func(c Conn, t time.Time) error						    // 测试空闲线程是否正常运行的函数，t是连接被放回连接池的时间

	// Maximum number of idle connections in the pool.
	MaxIdle int															   // 最大的空闲连接数

	// Maximum number of connections allocated by the pool at a given time.
	// When zero, there is no limit on the number of connections in the pool.
	MaxActive int														// 最大的活跃连接数，等价于 连接池的最大连接数

	// Close connections after remaining idle for this duration. If the value
	// is zero, then idle connections are not closed. Applications should set
	// the timeout to a value less than the server's timeout.
	IdleTimeout time.Duration											// 空闲连接的超时时间，连接被放回连接池开始计算

	// If Wait is true and the pool is at the MaxActive limit, then Get() waits
	// for a connection to be returned to the pool before returning.
	Wait bool															// 当没有空闲连接时，请求是否阻塞等待空闲连接

	// Close connections older than this duration. If the value is zero, then
	// the pool does not close connections based on age.
	MaxConnLifetime time.Duration										// 连接池中的链接的最大存活时间，从连接创建时开始计算

	chInitialized uint32 // set to 1 when field ch is initialized		// 这个队列用来控制最大连接数的上限，以及当没有空闲连接时，让当前获取连接的请求阻塞等待，前提是 Wait是true

	mu           sync.Mutex    // mu protects the following fields
	closed       bool          // set to true when the pool is closed.	// 连接池是否关闭
	active       int           // the number of open connections in the pool		// 连接池的当前连接总数
	ch           chan struct{} // limits open connections when p.Wait is true
	idle         idleList      // idle connections									// 空闲连接列表
	waitCount    int64         // total number of connections waited for.			// 阻塞等待空闲连接的数量
	waitDuration time.Duration // total time waited for new connections.			// 所有阻塞等待请求的等待总时长
}

// NewPool creates a new pool.
//
// Deprecated: Initialize the Pool directly as shown in the example.
func NewPool(newFn func() (Conn, error), maxIdle int) *Pool {
	return &Pool{Dial: newFn, MaxIdle: maxIdle}
}

// Get gets a connection. The application must close the returned connection.
// This method always returns a valid connection so that applications can defer
// error handling to the first use of the connection. If there is an error
// getting an underlying connection, then the connection Err, Do, Send, Flush
// and Receive methods return that error.
// 如果获取连接出错，会返回 errorConn，
// errorConn也是一个连接，只不过在调用 Do, Send, Flush 等方法时，才返回连接出错的错误
// 这样调用方就不用处理空指针异常
func (p *Pool) Get() Conn {
	pc, err := p.get(nil)
	if err != nil {
		return errorConn{err}
	}
	return &activeConn{p: p, pc: pc}
}

// GetContext gets a connection using the provided context.
//
// The provided Context must be non-nil. If the context expires before the
// connection is complete, an error is returned. Any expiration on the context
// will not affect the returned connection.
//
// If the function completes without error, then the application must close the
// returned connection.
func (p *Pool) GetContext(ctx context.Context) (Conn, error) {
	pc, err := p.get(ctx)
	if err != nil {
		return errorConn{err}, err
	}
	return &activeConn{p: p, pc: pc}, nil
}

// PoolStats contains pool statistics.
type PoolStats struct {
	// ActiveCount is the number of connections in the pool. The count includes
	// idle connections and connections in use.
	ActiveCount int
	// IdleCount is the number of idle connections in the pool.
	IdleCount int

	// WaitCount is the total number of connections waited for.
	// This value is currently not guaranteed to be 100% accurate.
	WaitCount int64

	// WaitDuration is the total time blocked waiting for a new connection.
	// This value is currently not guaranteed to be 100% accurate.
	WaitDuration time.Duration
}

// Stats returns pool's statistics.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	stats := PoolStats{
		ActiveCount:  p.active,
		IdleCount:    p.idle.count,
		WaitCount:    p.waitCount,
		WaitDuration: p.waitDuration,
	}
	p.mu.Unlock()

	return stats
}

// ActiveCount returns the number of connections in the pool. The count
// includes idle connections and connections in use.
func (p *Pool) ActiveCount() int {
	p.mu.Lock()
	active := p.active
	p.mu.Unlock()
	return active
}

// IdleCount returns the number of idle connections in the pool.
func (p *Pool) IdleCount() int {
	p.mu.Lock()
	idle := p.idle.count
	p.mu.Unlock()
	return idle
}

// Close releases the resources used by the pool.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.active -= p.idle.count
	pc := p.idle.front
	p.idle.count = 0
	p.idle.front, p.idle.back = nil, nil
	if p.ch != nil {
		close(p.ch)
	}
	p.mu.Unlock()
	for ; pc != nil; pc = pc.next {
		pc.c.Close()
	}
	return nil
}

func (p *Pool) lazyInit() {
	// Fast path.
	if atomic.LoadUint32(&p.chInitialized) == 1 {
		return
	}
	// Slow path.
	p.mu.Lock()
	if p.chInitialized == 0 {
		// p.MaxActive 最大活跃线程数
		p.ch = make(chan struct{}, p.MaxActive)
		if p.closed {
			close(p.ch)
		} else {
			for i := 0; i < p.MaxActive; i++ {
				// 这里可以理解为，最多只能从连接池取 MaxActive 个连接，超过会阻塞
				// 这里的取包括：1.从空闲连接列表取 2.新创建【也可理解为取，因为用完还是放回空闲连接列表】
				p.ch <- struct{}{}
			}
		}
		// ch 初始化完成
		atomic.StoreUint32(&p.chInitialized, 1)
	}
	p.mu.Unlock()
}

// get prunes stale connections and returns a connection from the idle list or
// creates a new connection.
// 从连接池中获取连接，有空闲连接，则取空闲连接；没有则创建新的连接
func (p *Pool) get(ctx context.Context) (*poolConn, error) {

	// Handle limit for p.Wait == true.
	var waited time.Duration

	// 开启客户端阻塞等待，而且设置了连接池的最大连接数
	if p.Wait && p.MaxActive > 0 {
		p.lazyInit()

		// wait indicates if we believe it will block so its not 100% accurate
		// however for stats it should be good enough.
		wait := len(p.ch) == 0   // ch 是否为空
		var start time.Time
		if wait {
			start = time.Now()
		}
		// 从 ch 读取一个元素，如果 ch 为空，这里会阻塞
		// 也就是说，前 MaxActive个请求，都不会阻塞，因为连接池的最大连接数是 MaxActive
		// -- 客户端阻塞等待，这里控制了连接池的连接数量上限
		if ctx == nil {
			<-p.ch
		} else {
			select {
			case <-p.ch:
			case <-ctx.Done():          // ctx中设置了超时
				return nil, ctx.Err()
			}
		}
		if wait {
			// 统计等待时间
			waited = time.Since(start)
		}
	}

	p.mu.Lock()
	// waited > 0 说明有客户端在等待连接
	if waited > 0 {
		p.waitCount++				// 等待连接的客户端请求数
		p.waitDuration += waited    // 统计等待客户端的总等待时间
	}

	// Prune stale connections at the back of the idle list.
	// 在空闲列表的后面修剪陈旧的连接。
	if p.IdleTimeout > 0 {
		// 设置了空间连接的超时时间

		// 空闲连接总数
		n := p.idle.count
		// 遍历空闲连接列表中的每个连接，将超时的空闲连接关闭
		for i := 0; i < n && p.idle.back != nil && p.idle.back.t.Add(p.IdleTimeout).Before(nowFunc()); i++ {
			// p.idle.back != nil && p.idle.back.t.Add(p.IdleTimeout).Before(nowFunc()): 空闲连接超时，即距离空闲连接被放回连接池的时间 > 空闲连接超时时间

			// 暂存空闲列表尾部连接
			pc := p.idle.back
			// 弹出空闲列表尾部的连接
			p.idle.popBack()

			p.mu.Unlock()

			// 关闭超时连接
			pc.c.Close()

			p.mu.Lock()
			// 连接总数【即连接池中的连接总数】 -1
			p.active--
		}
	}

	// Get idle connection from the front of idle list.
	// 从空闲列表的前面获取空闲连接。
	for p.idle.front != nil {
		pc := p.idle.front
		p.idle.popFront()

		p.mu.Unlock()

		// 连接有效性测试，对应的是: TestOnBorrow: ping
		if (p.TestOnBorrow == nil || p.TestOnBorrow(pc.c, pc.t) == nil) &&
			(p.MaxConnLifetime == 0 || nowFunc().Sub(pc.created) < p.MaxConnLifetime) {
			// 获取连接成功
			return pc, nil
		}

		// 连接失效，关闭连接
		pc.c.Close()
		p.mu.Lock()
		p.active--
	}

	// Check for pool closed before dialing a new connection.
	// 重新获取 Redis连接前，判断连接池是否关闭
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("redigo: get on closed pool")
	}

	// Handle limit for p.Wait == false.
	// 客户端不阻塞等待，设置了连接池上限，且已经达到了连接池上限，这里返回
	// -- 客户端不阻塞，这里控制了连接池的连接数量上限
	if !p.Wait && p.MaxActive > 0 && p.active >= p.MaxActive {
		p.mu.Unlock()
		return nil, ErrPoolExhausted
	}

	// 连接池的连接总数+1，因为接下来会重新创建与Redis的连接
	p.active++
	p.mu.Unlock()

	// 创建与Redis的新连接
	c, err := p.dial(ctx)
	// 获取连接失败
	if err != nil {
		c = nil
		p.mu.Lock()
		// 连接池的连接总数-1
		p.active--
		if p.ch != nil && !p.closed {
			// 因为前面从 ch 读了一个元素，这里创建失败，需要把这个元素放回去，让后面阻塞等待的请求，能继续创建新的Redis连接
			// 这里可以理解为没取成功
			p.ch <- struct{}{}
		}
		p.mu.Unlock()
	}
	// err 交给调用者处理了

	// 新创建的连接没有放到 p.idle，应该是连接关闭的时候再放
	return &poolConn{c: c, created: nowFunc()}, err
}

func (p *Pool) dial(ctx context.Context) (Conn, error) {
	if p.DialContext != nil {
		return p.DialContext(ctx)
	}
	// 这里的Dial，就是在 client.go:463 中的 initPool 方法中，初始化 p 对象的时候设置的
	// 见：
	/*
	Dial: func() (c redis.Conn, err error) {
		now := time.Now().Unix()
		// 每3秒才会重试一次
		if now < atomic.LoadInt64(&p.lastDialFail)+retryPeriod {
			err = fmt.Errorf("dial fail retry limit")
			return
		}
		c, err = dial(addr, p.connTimeout, p.readTimeout, p.writeTimeout)
		if err != nil {
			// 记录当前获取连接失败的时间戳，用于限制重试频率
			atomic.StoreInt64(&p.lastDialFail, now)
		} else {
			atomic.StoreInt64(&p.lastDialFail, 0)
		}
		return
	},
	*/
	// 这个方法会使用 提供的Redis的地址，返回与Redis的连接
	if p.Dial != nil {
		return p.Dial()
	}
	return nil, errors.New("redigo: must pass Dial or DialContext to pool")
}

func (p *Pool) put(pc *poolConn, forceClose bool) error {
	p.mu.Lock()
	if !p.closed && !forceClose {
		pc.t = nowFunc()
		p.idle.pushFront(pc)
		if p.idle.count > p.MaxIdle {
			// 大于空闲活跃连接上线，则把尾部的去掉
			pc = p.idle.back
			p.idle.popBack()
		} else {
			pc = nil
		}
	}

	if pc != nil {
		p.mu.Unlock()
		pc.c.Close()
		p.mu.Lock()
		p.active--
	}

	if p.ch != nil && !p.closed {
		// 用完将连接放回空闲连接列表之后，要往ch里加一个元素
		// 让后面的阻塞等待的客户端请求，可以继续取连接
		p.ch <- struct{}{}
	}
	p.mu.Unlock()
	return nil
}

type activeConn struct {
	p     *Pool
	pc    *poolConn
	state int
}

var (
	sentinel     []byte
	sentinelOnce sync.Once
)

func initSentinel() {
	p := make([]byte, 64)
	if _, err := rand.Read(p); err == nil {
		sentinel = p
	} else {
		h := sha1.New()
		io.WriteString(h, "Oops, rand failed. Use time instead.")
		io.WriteString(h, strconv.FormatInt(time.Now().UnixNano(), 10))
		sentinel = h.Sum(nil)
	}
}

func (ac *activeConn) Close() error {
	pc := ac.pc
	if pc == nil {
		return nil
	}
	ac.pc = nil

	if ac.state&connectionMultiState != 0 {
		pc.c.Send("DISCARD")
		ac.state &^= (connectionMultiState | connectionWatchState)
	} else if ac.state&connectionWatchState != 0 {
		pc.c.Send("UNWATCH")
		ac.state &^= connectionWatchState
	}
	if ac.state&connectionSubscribeState != 0 {
		pc.c.Send("UNSUBSCRIBE")
		pc.c.Send("PUNSUBSCRIBE")
		// To detect the end of the message stream, ask the server to echo
		// a sentinel value and read until we see that value.
		sentinelOnce.Do(initSentinel)
		pc.c.Send("ECHO", sentinel)
		pc.c.Flush()
		for {
			p, err := pc.c.Receive()
			if err != nil {
				break
			}
			if p, ok := p.([]byte); ok && bytes.Equal(p, sentinel) {
				ac.state &^= connectionSubscribeState
				break
			}
		}
	}
	pc.c.Do("")
	ac.p.put(pc, ac.state != 0 || pc.c.Err() != nil)
	return nil
}

func (ac *activeConn) Err() error {
	pc := ac.pc
	if pc == nil {
		return errConnClosed
	}
	return pc.c.Err()
}

func (ac *activeConn) Do(commandName string, args ...interface{}) (reply interface{}, err error) {
	pc := ac.pc
	if pc == nil {
		return nil, errConnClosed
	}
	ci := lookupCommandInfo(commandName)
	ac.state = (ac.state | ci.Set) &^ ci.Clear
	return pc.c.Do(commandName, args...)
}

func (ac *activeConn) DoWithTimeout(timeout time.Duration, commandName string, args ...interface{}) (reply interface{}, err error) {
	pc := ac.pc
	if pc == nil {
		return nil, errConnClosed
	}
	cwt, ok := pc.c.(ConnWithTimeout)
	if !ok {
		return nil, errTimeoutNotSupported
	}
	ci := lookupCommandInfo(commandName)
	ac.state = (ac.state | ci.Set) &^ ci.Clear
	return cwt.DoWithTimeout(timeout, commandName, args...)
}

func (ac *activeConn) Send(commandName string, args ...interface{}) error {
	pc := ac.pc
	if pc == nil {
		return errConnClosed
	}
	ci := lookupCommandInfo(commandName)
	ac.state = (ac.state | ci.Set) &^ ci.Clear
	return pc.c.Send(commandName, args...)
}

func (ac *activeConn) Flush() error {
	pc := ac.pc
	if pc == nil {
		return errConnClosed
	}
	return pc.c.Flush()
}

func (ac *activeConn) Receive() (reply interface{}, err error) {
	pc := ac.pc
	if pc == nil {
		return nil, errConnClosed
	}
	return pc.c.Receive()
}

func (ac *activeConn) ReceiveWithTimeout(timeout time.Duration) (reply interface{}, err error) {
	pc := ac.pc
	if pc == nil {
		return nil, errConnClosed
	}
	cwt, ok := pc.c.(ConnWithTimeout)
	if !ok {
		return nil, errTimeoutNotSupported
	}
	return cwt.ReceiveWithTimeout(timeout)
}

type errorConn struct{ err error }

func (ec errorConn) Do(string, ...interface{}) (interface{}, error) { return nil, ec.err }
func (ec errorConn) DoWithTimeout(time.Duration, string, ...interface{}) (interface{}, error) {
	return nil, ec.err
}
func (ec errorConn) Send(string, ...interface{}) error                     { return ec.err }
func (ec errorConn) Err() error                                            { return ec.err }
func (ec errorConn) Close() error                                          { return nil }
func (ec errorConn) Flush() error                                          { return ec.err }
func (ec errorConn) Receive() (interface{}, error)                         { return nil, ec.err }
func (ec errorConn) ReceiveWithTimeout(time.Duration) (interface{}, error) { return nil, ec.err }

type idleList struct {
	count       int
	front, back *poolConn
}

type poolConn struct {
	c          Conn
	t          time.Time  // 调用Close方法，连接放回连接池的时间
	created    time.Time  // 连接的创建时间
	next, prev *poolConn
}

func (l *idleList) pushFront(pc *poolConn) {
	pc.next = l.front
	pc.prev = nil
	if l.count == 0 {
		l.back = pc
	} else {
		l.front.prev = pc
	}
	l.front = pc
	l.count++
	return
}

func (l *idleList) popFront() {
	pc := l.front
	l.count--
	if l.count == 0 {
		l.front, l.back = nil, nil
	} else {
		pc.next.prev = nil
		l.front = pc.next
	}
	pc.next, pc.prev = nil, nil
}

func (l *idleList) popBack() {
	pc := l.back
	l.count--
	if l.count == 0 {
		l.front, l.back = nil, nil
	} else {
		pc.prev.next = nil
		l.back = pc.prev
	}
	pc.next, pc.prev = nil, nil
}
