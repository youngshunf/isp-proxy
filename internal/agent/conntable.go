package agent

import "net"

// connSet 是一个 GOST 服务上的存量连接，附带按源地址的索引（A-W6）。
//
// SOCKS5 认证器只拿得到源地址（见 connBySrc），连接数大时线性扫描会在
// Agent.mu 下停留很久，阻塞其它订阅的认证与登记。
type connSet struct {
	all map[*trackedConn]struct{}
	// 同一服务上同一源地址理论上可能出现多条连接（服务监听多个本地地址时，
	// TCP 四元组仍然唯一）。这种情况下无法确定认证属于哪条连接，查找返回空，
	// 认证随之失败；绝不任选一条，否则认证结果会记到另一条连接上。
	bySrc map[string][]*trackedConn
}

func (s *connSet) add(c *trackedConn) {
	s.all[c] = struct{}{}
	if c.src != "" {
		s.bySrc[c.src] = append(s.bySrc[c.src], c)
	}
}

func (s *connSet) remove(c *trackedConn) {
	if _, ok := s.all[c]; !ok {
		return
	}
	delete(s.all, c)
	list := s.bySrc[c.src]
	for i, x := range list {
		if x == c {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(s.bySrc, c.src)
	} else {
		s.bySrc[c.src] = list
	}
}

// connsLocked 返回服务上的存量连接集合（可能为空），调用方须持有 a.mu。
func (a *Agent) connsLocked(service string) map[*trackedConn]struct{} {
	if s := a.conns[service]; s != nil {
		return s.all
	}
	return nil
}

// connBySrc 按服务名和客户端地址在连接表中查找连接。
// SOCKS5 处理器以 context.Background() 调用认证器，只附带源地址（x v0.28.1 socks/v5/selector.go）。
// 源地址不唯一时返回 nil。
func (a *Agent) connBySrc(service string, src net.Addr) *trackedConn {
	if src == nil {
		return nil
	}
	key := src.String()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.conns[service]
	if s == nil {
		return nil
	}
	if list := s.bySrc[key]; len(list) == 1 {
		return list[0]
	}
	return nil
}

// track 登记连接，并根据服务名确定所属订阅。
func (a *Agent) track(c *trackedConn) {
	a.mu.Lock()
	if id, ok := a.services[c.service]; ok {
		c.sub = a.subs[id]
	}
	s := a.conns[c.service]
	if s == nil {
		s = &connSet{all: map[*trackedConn]struct{}{}, bySrc: map[string][]*trackedConn{}}
		a.conns[c.service] = s
	}
	s.add(c)
	a.mu.Unlock()
}

func (a *Agent) untrack(c *trackedConn) {
	a.mu.Lock()
	if s := a.conns[c.service]; s != nil {
		s.remove(c)
	}
	a.mu.Unlock()
}
